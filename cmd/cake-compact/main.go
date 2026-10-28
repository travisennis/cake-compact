// Command cake-compact is a proof-of-concept external compactor for Cake.
//
// It runs normal cake turns, reads the main-agent context measurement from the
// session, summarizes the retired prefix when the context crosses a threshold,
// and appends one checkpoint to the existing session through
// `cake sessions checkpoint`. The session id stays stable and raw history
// stays on disk. An explicit -fork flag keeps the older minimal-session writer
// as an escape hatch. Nothing in Cake changes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/travisennis/cake-compact/internal/cakecli"
	"github.com/travisennis/cake-compact/internal/compact"
	"github.com/travisennis/cake-compact/internal/session"
	"github.com/travisennis/cake-compact/internal/state"
)

func main() {
	os.Exit(runCLI(os.Args[1:]))
}

func runCLI(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "run":
		return commandRun(args[1:])
	case "compact":
		return commandCompact(args[1:])
	case "inspect":
		return commandInspect(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	case "version", "-V", "--version":
		fmt.Println("cake-compact 0.1.0 (proof of concept)")
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cake-compact - external context compaction for cake (proof of concept)

Usage:
  cake-compact run [flags] <prompt...>     run one cake turn, compacting when over threshold
  cake-compact compact [flags] <session>   compact an existing session in place
  cake-compact inspect [flags] <session>   show the context measurement and compaction plan

The session argument is a UUID (resolved under the cake sessions directory) or a
path to a .jsonl session file. Compaction appends one checkpoint to the source
session through 'cake sessions checkpoint' and keeps its session id, so the
next turn resumes the same session. Pass -fork to write a new minimal session
file instead (explicit escape hatch; the session id then changes).

Flags shared by all commands:
  -cake-bin <path>            cake executable (default: $CAKE_BIN or cake)
  -data-dir <path>            cake data root (default: $CAKE_DATA_DIR or ~/.local/share/cake)
  -fork                       write a new minimal session instead of appending in place
  -summary-file <path>        use a fixed summary instead of calling a model
  -summarizer-command <cmd>   shell command: transcript on stdin, summary on stdout
  -summarizer-model <name>    model for the default cake summarizer
  -summarizer-profile <name>  profile for the default cake summarizer
  -summarizer-reasoning-effort <level>
                              cake reasoning effort for the summarizer
  -instructions <text>        extra focus for the summary prompt
  -threshold-percent <n>      compact at this percent of the window (default 80)
  -threshold-tokens <n>       explicit trigger instead of a window percentage
  -window <n>                 override the context window from the session
  -keep-tokens <n>            retained-tail target (default 20000 or window/4)
  -min-reduction-percent <n>  minimum estimated reduction (default 10)
  -force                      ignore the threshold and minimum reduction
  -dry-run                    plan without calling the model or writing
  -no-verify                  skip the final cake replay validation (fork only)
  -timeout <duration>         summary and verification deadline (default 5m)
  -json                       machine-readable output
  -verbose                    show plan, compaction, and stage timing detail on stderr

Run-only flags:
  -resume <uuid>              resume this session instead of the saved one
  -state <path>               per-directory state file
  -no-state                   do not read or write the state file
  -cwd <path>                 run cake in this directory
  -model <name>               cake model for the turn
  -profile <name>             cake profile for the turn
  -reasoning-effort <level>   cake reasoning effort for the turn
  -add-dir <path>             pass through to cake (repeatable)
  -cake-arg <arg>             pass a raw argument to cake (repeatable)
  -no-compact                 run the turn without considering compaction
  -force-compact              compact after the turn regardless of threshold

Examples:
  cake-compact run "fix the failing test"
  cake-compact inspect 0d8f6a1e-...
  cake-compact compact -dry-run 0d8f6a1e-...
  cake-compact compact -summary-file notes.txt 0d8f6a1e-...
  cake-compact compact -force -keep-tokens 12000 0d8f6a1e-...
  cake-compact compact -fork -summary-file notes.txt 0d8f6a1e-...
`)
}

type commonFlags struct {
	cakeBin                   string
	dataDir                   string
	forkMode                  bool
	summaryFile               string
	summarizerCommand         string
	summarizerModel           string
	summarizerProfile         string
	summarizerReasoningEffort string
	instructions              string
	thresholdPercent          int
	thresholdTokens           uint64
	window                    uint64
	keepTokens                uint64
	minReduction              int
	force                     bool
	dryRun                    bool
	jsonOut                   bool
	verbose                   bool
	noVerify                  bool
	timeout                   time.Duration
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.cakeBin, "cake-bin", "", "cake executable")
	fs.StringVar(&c.dataDir, "data-dir", "", "cake data root")
	fs.BoolVar(&c.forkMode, "fork", false, "write a new minimal session instead of appending in place")
	fs.StringVar(&c.summaryFile, "summary-file", "", "use a fixed summary file")
	fs.StringVar(&c.summarizerCommand, "summarizer-command", "", "shell summarizer command")
	fs.StringVar(&c.summarizerModel, "summarizer-model", "", "model for the cake summarizer")
	fs.StringVar(&c.summarizerProfile, "summarizer-profile", "", "profile for the cake summarizer")
	fs.StringVar(&c.summarizerReasoningEffort, "summarizer-reasoning-effort", "", "reasoning effort for the cake summarizer")
	fs.StringVar(&c.instructions, "instructions", "", "extra summary focus")
	fs.IntVar(&c.thresholdPercent, "threshold-percent", 80, "trigger percent of the window")
	fs.Uint64Var(&c.thresholdTokens, "threshold-tokens", 0, "explicit trigger token count")
	fs.Uint64Var(&c.window, "window", 0, "override context window")
	fs.Uint64Var(&c.keepTokens, "keep-tokens", 0, "retained tail target")
	fs.IntVar(&c.minReduction, "min-reduction-percent", 10, "minimum estimated reduction")
	fs.BoolVar(&c.force, "force", false, "ignore threshold and minimum reduction")
	fs.BoolVar(&c.dryRun, "dry-run", false, "plan without calling the model or writing")
	fs.BoolVar(&c.jsonOut, "json", false, "machine-readable output")
	fs.BoolVar(&c.verbose, "verbose", false, "verbose stderr")
	fs.BoolVar(&c.noVerify, "no-verify", false, "skip replay verification")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Minute, "summary and verification deadline")
}

func (c *commonFlags) options(trigger string, force bool) compact.Options {
	return compact.Options{
		ThresholdPercent:    c.thresholdPercent,
		ThresholdTokens:     c.thresholdTokens,
		Window:              c.window,
		KeepTokens:          c.keepTokens,
		MinReductionPercent: c.minReduction,
		Force:               force,
		DryRun:              c.dryRun,
		Trigger:             trigger,
		SummaryFile:         c.summaryFile,
		Instructions:        c.instructions,
	}
}

func (c *commonFlags) compactor(trigger string, force bool) (*compact.Compactor, string, error) {
	dataDir, err := cakecli.ResolveDataDir(c.dataDir)
	if err != nil {
		return nil, "", err
	}
	binary, err := cakecli.ResolveBinary(c.cakeBin)
	if err != nil {
		return nil, "", err
	}

	var summarizer compact.Summarizer
	switch {
	case c.summaryFile != "":
		summarizer = compact.FileSummarizer{Path: c.summaryFile}
	case c.summarizerCommand != "":
		summarizer = compact.CommandSummarizer{Command: c.summarizerCommand}
	default:
		summarizer = compact.CakeSummarizer{
			Binary:          binary,
			Model:           c.summarizerModel,
			Profile:         c.summarizerProfile,
			ReasoningEffort: c.summarizerReasoningEffort,
			Instructions:    c.instructions,
		}
	}

	compactor := &compact.Compactor{
		Mode:       compact.ModeInPlace,
		DataDir:    dataDir,
		CakeBin:    binary,
		Options:    c.options(trigger, force),
		Summarizer: summarizer,
		Verify:     !c.noVerify && !c.dryRun,
		Now:        time.Now,
		NewID:      compact.NewUUID,
	}
	if c.forkMode {
		compactor.Mode = compact.ModeFork
	}
	if c.verbose {
		compactor.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "cake-compact: "+format+"\n", args...)
		}
	}
	return compactor, dataDir, nil
}

func commandCompact(args []string) int {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var flags commonFlags
	flags.register(fs)
	onlyWhenNeeded := fs.Bool("threshold", false, "only compact when the threshold is crossed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "compact requires one session UUID or path")
		return 2
	}

	compactor, dataDir, err := flags.compactor("manual", !*onlyWhenNeeded || flags.force)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	path := resolveSessionPath(fs.Arg(0), dataDir)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(os.Stderr, "cake-compact: session not found: %s\n", path)
		return 1
	}

	ctx, cancel := signalTimeout(flags.timeout)
	defer cancel()
	result, err := compactor.Compact(ctx, path)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			fmt.Fprintln(os.Stderr, "cake-compact: interrupted")
			return 130
		}
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	if flags.verbose {
		logTimings(result.Timings)
	}
	return printCompactionResult(result, flags.jsonOut)
}

func commandInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var flags commonFlags
	flags.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "inspect requires one session UUID or path")
		return 2
	}

	dataDir, err := cakecli.ResolveDataDir(flags.dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	path := resolveSessionPath(fs.Arg(0), dataDir)
	started := time.Now()
	file, err := session.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	loadMs := time.Since(started).Milliseconds()
	planStarted := time.Now()
	plan, decision, err := compact.Analyze(file, flags.options("threshold", flags.force))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	planMs := time.Since(planStarted).Milliseconds()

	checkpointID := ""
	if index := file.LatestCheckpointIndex(); index >= 0 {
		checkpointID = file.Records[index].Checkpoint.CheckpointID
	}
	result := inspectResult{
		SessionID:       file.SessionID(),
		SessionFile:     file.Path,
		CheckpointID:    checkpointID,
		EffectiveItems:  len(plan.Projected),
		PrefixItems:     len(plan.Prefix),
		RetainedItems:   len(plan.Retained),
		AnchorItem:      plan.Anchor,
		PinnedRequest:   plan.Anchor >= 0,
		BeforeTokens:    plan.BeforeTokens,
		RetainedTokens:  plan.RetainedTokens,
		Window:          plan.Window,
		ThresholdTokens: plan.ThresholdTokens,
		KeepTarget:      plan.KeepTarget,
		Basis:           plan.Basis,
		Compact:         decision.Compact,
		Reason:          decision.Reason,
		Timings: &compact.Timings{
			TotalMs: time.Since(started).Milliseconds(),
			LoadMs:  &loadMs,
			PlanMs:  &planMs,
		},
	}
	if flags.verbose {
		logTimings(result.Timings)
	}
	if flags.jsonOut {
		return writeJSON(result)
	}

	fmt.Printf("session: %s\n", result.SessionID)
	fmt.Printf("file: %s\n", result.SessionFile)
	if result.CheckpointID != "" {
		fmt.Printf("latest checkpoint: %s\n", result.CheckpointID)
	}
	fmt.Printf("estimated input: %d tokens (%s)\n", result.BeforeTokens, result.Basis)
	if result.Window > 0 {
		fmt.Printf("context window: %d tokens\n", result.Window)
		fmt.Printf("threshold: %d tokens (%d%%)\n", result.ThresholdTokens, flags.thresholdPercent)
	} else if result.ThresholdTokens > 0 {
		fmt.Printf("threshold: %d tokens\n", result.ThresholdTokens)
	} else {
		fmt.Println("context window: unknown (use -window or -threshold-tokens)")
	}
	fmt.Printf("effective items: %d (prefix %d, retained %d)\n", result.EffectiveItems, result.PrefixItems, result.RetainedItems)
	if result.PinnedRequest {
		fmt.Printf("pinned request: item %d\n", result.AnchorItem)
	} else {
		fmt.Println("pinned request: none (no real user message at or before the tail)")
	}
	fmt.Printf("retained estimate: %d tokens (target %d)\n", result.RetainedTokens, result.KeepTarget)
	if result.Compact {
		fmt.Println("would compact: yes")
	} else {
		fmt.Printf("would compact: no (%s)\n", result.Reason)
	}
	return 0
}

type inspectResult struct {
	SessionID       string           `json:"session_id"`
	SessionFile     string           `json:"session_file"`
	CheckpointID    string           `json:"checkpoint_id,omitempty"`
	EffectiveItems  int              `json:"effective_items"`
	PrefixItems     int              `json:"prefix_items"`
	RetainedItems   int              `json:"retained_items"`
	AnchorItem      int              `json:"anchor_item"`
	PinnedRequest   bool             `json:"pinned_request"`
	BeforeTokens    uint64           `json:"before_tokens"`
	RetainedTokens  uint64           `json:"retained_tokens"`
	Window          uint64           `json:"window,omitempty"`
	ThresholdTokens uint64           `json:"threshold_tokens,omitempty"`
	KeepTarget      uint64           `json:"keep_target"`
	Basis           string           `json:"basis"`
	Compact         bool             `json:"compact"`
	Reason          string           `json:"reason"`
	Timings         *compact.Timings `json:"timings,omitempty"`
}

func commandRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var flags commonFlags
	flags.register(fs)

	var (
		resume       string
		statePath    string
		noState      bool
		cwd          string
		model        string
		profile      string
		reasoning    string
		addDirs      []string
		cakeArgs     []string
		noCompact    bool
		forceCompact bool
	)
	fs.StringVar(&resume, "resume", "", "resume a session UUID")
	fs.StringVar(&statePath, "state", "", "state file")
	fs.BoolVar(&noState, "no-state", false, "do not read or write state")
	fs.StringVar(&cwd, "cwd", "", "working directory")
	fs.StringVar(&model, "model", "", "cake model")
	fs.StringVar(&profile, "profile", "", "cake profile")
	fs.StringVar(&reasoning, "reasoning-effort", "", "cake reasoning effort")
	fs.Func("add-dir", "add a read-only directory (repeatable)", func(value string) error {
		addDirs = append(addDirs, value)
		return nil
	})
	fs.Func("cake-arg", "raw cake argument (repeatable)", func(value string) error {
		cakeArgs = append(cakeArgs, value)
		return nil
	})
	fs.BoolVar(&noCompact, "no-compact", false, "do not compact after the turn")
	fs.BoolVar(&forceCompact, "force-compact", false, "compact regardless of threshold")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	promptFromStdin := false
	prompt := strings.Join(fs.Args(), " ")
	if prompt == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cake-compact: read stdin:", err)
			return 1
		}
		prompt = strings.TrimSpace(string(data))
		promptFromStdin = true
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "cake-compact: a prompt is required")
		return 2
	}

	var (
		workingDir string
		err        error
	)
	if cwd == "" {
		workingDir, err = os.Getwd()
	} else {
		workingDir, err = filepath.Abs(cwd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}

	// Install the signal handler before the turn so Ctrl-C reaches the cake
	// child (same process group) instead of killing this wrapper outright. The
	// child handles its own interrupt; a signal also cancels any compaction
	// work that follows.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	dataDir, err := cakecli.ResolveDataDir(flags.dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	binary, err := cakecli.ResolveBinary(flags.cakeBin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}

	// Fail before the turn when the binary cannot append a checkpoint. The
	// turn would otherwise succeed and then be discarded after spending a
	// metered model call. -no-compact and -fork do not need the verb.
	if !noCompact && !flags.forkMode {
		if err := cakecli.CheckpointAvailable(signalCtx, binary); err != nil {
			fmt.Fprintln(os.Stderr, "cake-compact:", err)
			return 1
		}
	}

	// The default summarizer also needs Cake's source-level runtime-context
	// switch. Check it before the turn for the same reason as the verb above;
	// fixed summary files and commands do not use it.
	if !noCompact && !flags.dryRun && flags.summaryFile == "" && flags.summarizerCommand == "" {
		if err := cakecli.NoRuntimeContextAvailable(signalCtx, binary); err != nil {
			fmt.Fprintln(os.Stderr, "cake-compact:", err)
			return 1
		}
	}

	if statePath == "" && !noState {
		statePath, err = state.DefaultPath(workingDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cake-compact:", err)
			return 1
		}
	}
	resumeID := resume
	var savedState *state.State
	if statePath != "" {
		saved, err := state.Load(statePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cake-compact: ignoring state:", err)
		} else {
			savedState = saved
			if resumeID == "" && saved != nil {
				resumeID = saved.SessionID
			}
		}
	}
	if resumeID != "" {
		path := filepath.Join(cakecli.SessionsDir(dataDir), resumeID+".jsonl")
		if _, err := os.Stat(path); err != nil {
			if resume != "" {
				fmt.Fprintf(os.Stderr, "cake-compact: session not found: %s\n", path)
				return 1
			}
			fmt.Fprintf(os.Stderr, "cake-compact: saved session %s is gone; starting a new session\n", resumeID)
			resumeID = ""
		}
	}

	stdout, exitCode, err := cakecli.RunTurn(context.Background(), cakecli.TurnOptions{
		Binary:          binary,
		DataDir:         dataDir,
		WorkingDir:      workingDir,
		Resume:          resumeID,
		Model:           model,
		Profile:         profile,
		ReasoningEffort: reasoning,
		AddDirs:         addDirs,
		ExtraArgs:       cakeArgs,
		Prompt:          prompt,
		PromptFromStdin: promptFromStdin,
		Stderr:          os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}

	var completion cakecli.Completion
	if err := json.Unmarshal(stdout, &completion); err != nil {
		if len(stdout) > 0 {
			fmt.Fprint(os.Stdout, string(stdout))
		}
		fmt.Fprintf(os.Stderr, "cake-compact: could not parse cake completion JSON: %v\n", err)
		return 1
	}

	output := runOutput{
		SessionID:     completion.SessionID,
		NextSessionID: completion.SessionID,
		Result:        completion.Result,
	}
	if completion.Error != nil && strings.TrimSpace(*completion.Error) != "" {
		output.Error = strings.TrimSpace(*completion.Error)
	}
	if !flags.jsonOut {
		if completion.Result != nil {
			fmt.Println(*completion.Result)
		}
		if output.Error != "" {
			fmt.Fprintln(os.Stderr, output.Error)
		}
	}

	// Only rewrite the resume pointer when the id actually changes. An
	// in-place compaction leaves it alone.
	if completion.SessionID != "" && statePath != "" &&
		(savedState == nil || savedState.SessionID != completion.SessionID) {
		if err := state.Save(statePath, &state.State{
			SessionID:        completion.SessionID,
			WorkingDirectory: workingDir,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "cake-compact: save state:", err)
		}
	}

	compactionFailed := false
	compactionInterrupted := false
	if !noCompact && completion.SessionID != "" {
		path := filepath.Join(cakecli.SessionsDir(dataDir), completion.SessionID+".jsonl")
		if _, statErr := os.Stat(path); statErr == nil {
			trigger := "threshold"
			force := false
			if forceCompact {
				trigger = "manual"
				force = true
			}
			compactor, _, err := flags.compactor(trigger, force)
			if err != nil {
				compactionFailed = true
				output.CompactionError = err.Error()
				fmt.Fprintln(os.Stderr, "cake-compact: compaction skipped:", err)
			} else {
				ctx, cancel := context.WithTimeout(signalCtx, flags.timeout)
				result, compactErr := compactor.Compact(ctx, path)
				canceled := errors.Is(ctx.Err(), context.Canceled)
				cancel()
				switch {
				case compactErr != nil:
					compactionFailed = true
					compactionInterrupted = canceled
					output.CompactionError = compactErr.Error()
					if compactionInterrupted {
						fmt.Fprintln(os.Stderr, "cake-compact: compaction interrupted")
					} else {
						fmt.Fprintln(os.Stderr, "cake-compact: compaction failed:", compactErr)
					}
				case result.Compacted:
					output.Compaction = result
					output.NextSessionID = result.SessionID
					// In place the session id does not change, so the state file
					// already points at it. Only a fork moves the pointer.
					if statePath != "" && result.SessionID != completion.SessionID {
						if err := state.Save(statePath, &state.State{
							SessionID:        result.SessionID,
							WorkingDirectory: workingDir,
						}); err != nil {
							fmt.Fprintln(os.Stderr, "cake-compact: save state:", err)
						}
					}
					if !flags.jsonOut {
						if result.InPlace {
							fmt.Fprintf(
								os.Stderr,
								"cake-compact: compacted in place %s at boundary %d (%d -> %d estimated input tokens)\n",
								result.SessionID, result.Boundary, result.BeforeTokens, result.AfterTokens,
							)
						} else {
							fmt.Fprintf(
								os.Stderr,
								"cake-compact: compacted %s -> %s (%d -> %d estimated input tokens)\n",
								result.SourceSessionID, result.SessionID, result.BeforeTokens, result.AfterTokens,
							)
						}
					}
				default:
					output.Compaction = result
					if flags.verbose && !flags.jsonOut {
						fmt.Fprintf(os.Stderr, "cake-compact: no compaction: %s\n", result.Reason)
					}
				}
				if flags.verbose && result != nil {
					logTimings(result.Timings)
				}
			}
		}
	}

	if flags.jsonOut {
		if code := writeJSON(output); code != 0 {
			return code
		}
	}
	if compactionInterrupted {
		return 130
	}
	if exitCode != 0 {
		return exitCode
	}
	if output.Error != "" || compactionFailed {
		return 1
	}
	return 0
}

type runOutput struct {
	SessionID       string          `json:"session_id"`
	NextSessionID   string          `json:"next_session_id,omitempty"`
	Result          *string         `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	Compaction      *compact.Result `json:"compaction,omitempty"`
	CompactionError string          `json:"compaction_error,omitempty"`
}

func logTimings(timings *compact.Timings) {
	if timings == nil {
		return
	}
	var builder strings.Builder
	builder.WriteString("cake-compact: timings:")
	for _, stage := range []struct {
		name  string
		value *int64
	}{
		{"load", timings.LoadMs},
		{"plan", timings.PlanMs},
		{"preflight", timings.PreflightMs},
		{"summary", timings.SummaryMs},
		{"commit", timings.CommitMs},
		{"verify", timings.VerifyMs},
	} {
		if stage.value != nil {
			fmt.Fprintf(&builder, " %s=%dms", stage.name, *stage.value)
		}
	}
	fmt.Fprintf(&builder, " total=%dms", timings.TotalMs)
	fmt.Fprintln(os.Stderr, builder.String())
}

func printCompactionResult(result *compact.Result, jsonOut bool) int {
	if jsonOut {
		return writeJSON(result)
	}
	if result.Compacted {
		if result.InPlace {
			fmt.Printf("compacted in place: %s\n", result.SessionID)
		} else {
			fmt.Printf("compacted %s -> %s\n", result.SourceSessionID, result.SessionID)
		}
		fmt.Printf("  input: %d -> %d estimated tokens (%s)\n", result.BeforeTokens, result.AfterTokens, result.Basis)
		if result.SummaryUsage != nil {
			fmt.Printf("  summary: %d in / %d out tokens", result.SummaryUsage.InputTokens, result.SummaryUsage.OutputTokens)
			if result.SummaryTimeMs > 0 {
				fmt.Printf(" in %s", time.Duration(result.SummaryTimeMs)*time.Millisecond)
			}
			fmt.Println()
		}
		fmt.Printf("  retained: %d items (target %d tokens)\n", result.RetainedItems, result.KeepTarget)
		fmt.Printf("  checkpoint: %s at boundary %d\n", result.CheckpointID, result.Boundary)
		fmt.Printf("  session file: %s\n", result.SessionFile)
		fmt.Printf("  resume: cake --resume %s \"continue\"\n", result.SessionID)
		return 0
	}
	fmt.Printf("no compaction: %s\n", result.Reason)
	if result.BeforeTokens > 0 {
		fmt.Printf("  estimated input: %d tokens\n", result.BeforeTokens)
	}
	if result.RetainedItems > 0 {
		fmt.Printf("  retained: %d items (%d estimated tokens), target %d\n", result.RetainedItems, result.AfterTokens, result.KeepTarget)
	}
	return 0
}

// signalTimeout returns a context that is cancelled by SIGINT/SIGTERM or after
// the timeout, whichever comes first.
func signalTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(signalCtx, timeout)
	return ctx, func() {
		cancel()
		stop()
	}
}

func resolveSessionPath(argument, dataDir string) string {
	if strings.HasSuffix(argument, ".jsonl") || strings.ContainsRune(argument, os.PathSeparator) {
		return argument
	}
	return filepath.Join(cakecli.SessionsDir(dataDir), argument+".jsonl")
}

func writeJSON(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, "cake-compact:", err)
		return 1
	}
	return 0
}
