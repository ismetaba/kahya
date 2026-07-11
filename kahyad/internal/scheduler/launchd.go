// launchd.go renders LaunchAgent plists for config.Config.Jobs entries
// (task spec step 2) and syncs the ones actually bootstrapped into the
// user's GUI launchd session (gui/$UID — LaunchAgents, NEVER
// LaunchDaemons, HANDOFF §7 ⚑ TCC checklist: grants attach to the
// responsible process) to exactly match that list (task spec step 5).
//
// ALL paths in a rendered plist are absolute — launchd does not expand
// "~" — including the trigger binary's own path and the per-job log file
// path (StandardOutPath/StandardErrorPath both point at the same
// "job-<name>.log" file under ~/Library/Logs/Kahya, created if missing).
package scheduler

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"kahya/kahyad/internal/config"
	"kahya/kahyad/internal/logx"
)

//go:embed plist.tmpl
var plistTemplateSrc string

var plistTemplate = template.Must(template.New("plist").Parse(plistTemplateSrc))

// plistLabelPrefix/plistFileSuffix bound the "com.kahya.job.<name>.plist"
// filename convention: labelFor/plistFileName build it, jobNameFromPlistFile
// parses it back out — the ONLY files Sync's stale-job cleanup pass ever
// touches are ones matching this exact shape, so it can never mistake
// kahyad's OWN LaunchAgent (com.kahya.kahyad.plist, installed by `make
// install-agent`, living in the SAME ~/Library/LaunchAgents directory) for
// a stale job plist.
const (
	plistLabelPrefix = "com.kahya.job."
	plistFileSuffix  = ".plist"
)

// labelFor returns the launchd Label a job's LaunchAgent is bootstrapped
// under: "com.kahya.job.<name>".
func labelFor(jobName string) string {
	return plistLabelPrefix + jobName
}

// plistFileName returns the on-disk filename (not a full path) for a
// job's rendered plist.
func plistFileName(jobName string) string {
	return labelFor(jobName) + plistFileSuffix
}

// jobNameFromPlistFileName extracts the job name back out of a filename
// previously produced by plistFileName, or reports ok=false for any
// filename that doesn't match that exact "com.kahya.job.<name>.plist"
// shape (including kahyad's own "com.kahya.kahyad.plist").
func jobNameFromPlistFileName(fileName string) (name string, ok bool) {
	if !strings.HasPrefix(fileName, plistLabelPrefix) || !strings.HasSuffix(fileName, plistFileSuffix) {
		return "", false
	}
	name = strings.TrimSuffix(strings.TrimPrefix(fileName, plistLabelPrefix), plistFileSuffix)
	if name == "" {
		return "", false
	}
	return name, true
}

// calendarEntry is one StartCalendarInterval key/value pair the template
// emits, in the fixed order calendarEntries below produces.
type calendarEntry struct {
	Key   string
	Value int
}

// calendarEntries flattens a config.CalendarSpec into the ordered list of
// present (non-nil) StartCalendarInterval entries — a nil field is
// omitted entirely, matching launchd's own documented "absent key means
// every value of that unit" StartCalendarInterval semantics.
func calendarEntries(c config.CalendarSpec) []calendarEntry {
	var entries []calendarEntry
	if c.Minute != nil {
		entries = append(entries, calendarEntry{"Minute", *c.Minute})
	}
	if c.Hour != nil {
		entries = append(entries, calendarEntry{"Hour", *c.Hour})
	}
	if c.Day != nil {
		entries = append(entries, calendarEntry{"Day", *c.Day})
	}
	if c.Weekday != nil {
		entries = append(entries, calendarEntry{"Weekday", *c.Weekday})
	}
	return entries
}

// plistData is what plist.tmpl renders against.
type plistData struct {
	Label           string
	TriggerBinPath  string
	JobName         string
	CalendarEntries []calendarEntry
	LogPath         string
}

// RenderPlist renders job's LaunchAgent plist as a string. triggerBinPath
// must be the absolute path to the kahya-trigger binary; jobLogDir must be
// the absolute ~/Library/Logs/Kahya directory (both paths are embedded
// verbatim into the output — launchd never expands "~").
//
// MINOR 3 fix: every string value interpolated into plist.tmpl (the
// launchd Label, the trigger binary's path, the bare job name, the log
// file path) is passed through escapeXMLString first. plist.tmpl is a
// plain text/template — text/template performs NO escaping of any kind,
// unlike html/template — so without this, a job name (or any future
// caller-supplied value) containing an XML metacharacter (<, &, ", ')
// would emit malformed or injected plist XML. config.Load's jobNamePattern
// (DNS-label chars only) already prevents an XML metacharacter from ever
// reaching a job Name in the normal config.yaml -> Load -> RenderPlist
// path, so this is a defensive, belt-and-suspenders layer for RenderPlist
// itself, which is exported and could in principle be called directly
// with an un-validated JobConfig. Every value RenderPlist has ever
// actually rendered (DNS-label job names, absolute filesystem paths)
// contains none of the runes escapeXMLString rewrites, so this is a
// byte-identical no-op for all of them — the golden-file test
// (TestRenderPlistGolden) is unaffected.
func RenderPlist(job config.JobConfig, triggerBinPath, jobLogDir string) (string, error) {
	data := plistData{
		Label:           escapeXMLString(labelFor(job.Name)),
		TriggerBinPath:  escapeXMLString(triggerBinPath),
		JobName:         escapeXMLString(job.Name),
		CalendarEntries: calendarEntries(job.Calendar),
		LogPath:         escapeXMLString(filepath.Join(jobLogDir, "job-"+job.Name+".log")),
	}
	var buf bytes.Buffer
	if err := plistTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("scheduler: render plist for job %q: %w", job.Name, err)
	}
	return buf.String(), nil
}

// escapeXMLString XML-escapes s via encoding/xml.EscapeText — the same
// escaping the standard library's own xml.Marshal applies to character
// data — so a string interpolated into plist.tmpl's <string> elements can
// never produce malformed or injected XML (MINOR 3 fix; see RenderPlist's
// doc comment for the full rationale).
func escapeXMLString(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		// xml.EscapeText only ever errors on a write failure from the
		// underlying io.Writer — a bytes.Buffer's Write never fails, so
		// this branch is unreachable in practice. Fail safe (return the
		// raw string) rather than panic: config.Load's jobNamePattern
		// remains the primary enforcement layer regardless.
		return s
	}
	return buf.String()
}

// Runner executes one external command to completion, returning its
// combined output wrapped into the error on failure. Production callers
// use NewExecRunner (real launchctl); tests inject a fake to assert
// Sync's decision logic (which plists get written/removed, when
// bootout+bootstrap run) without a real GUI launchd session.
type Runner interface {
	Run(args ...string) error
}

// execRunner is the real Runner: it execs launchctl.
type execRunner struct{}

// NewExecRunner returns the Runner Sync should use in production: it
// shells out to the real `launchctl` binary on PATH.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SyncOptions bundles Sync's fixed inputs, independent of the jobs list
// itself.
type SyncOptions struct {
	// LaunchAgentsDir is ~/Library/LaunchAgents — created (0700) if
	// missing.
	LaunchAgentsDir string
	// JobLogDir is ~/Library/Logs/Kahya — created (0700) if missing (task
	// spec step 2: "the sync step creates ~/Library/Logs/Kahya/ if
	// missing").
	JobLogDir string
	// TriggerBinPath is the absolute path to the kahya-trigger binary
	// every rendered plist's ProgramArguments points at.
	TriggerBinPath string
}

// currentUIDFn resolves the numeric UID Sync addresses the GUI launchd
// domain with (gui/<uid>). A package-level var (not a bare os.Getuid()
// call at each use) purely so a test could override it if ever needed;
// production always uses the real os.Getuid().
var currentUIDFn = func() string { return strconv.Itoa(os.Getuid()) }

// Sync renders + installs/removes launchd LaunchAgent plists so the set
// of com.kahya.job.<name> LaunchAgents on disk (and bootstrapped into
// gui/$UID) exactly matches jobs (task spec step 5). It is idempotent: a
// job whose rendered plist content is UNCHANGED from what's already on
// disk is left completely alone — no bootout/bootstrap cycle, no
// launchctl call at all — so repeated Sync calls (every kahyad boot, plus
// every manual `kahyad -sync-jobs`) never needlessly restart an
// already-correct LaunchAgent. A job whose plist is new or changed gets
// `launchctl bootout` (errors ignored — "not loaded" is the expected,
// common case for a brand-new job) followed by `launchctl bootstrap`
// (this one's error DOES propagate — a failed install is a real
// problem). Any job no longer present in jobs has its plist file removed
// and is booted out, but ONLY for files matching the exact
// "com.kahya.job.<name>.plist" shape (jobNameFromPlistFileName) — kahyad's
// own com.kahya.kahyad.plist, installed separately by `make
// install-agent` into the very same directory, is never touched.
func Sync(jobs []config.JobConfig, opts SyncOptions, runner Runner, jsonl *logx.Logger) error {
	if err := os.MkdirAll(opts.LaunchAgentsDir, 0o700); err != nil {
		return fmt.Errorf("scheduler: create LaunchAgents dir %s: %w", opts.LaunchAgentsDir, err)
	}
	if err := os.MkdirAll(opts.JobLogDir, 0o700); err != nil {
		return fmt.Errorf("scheduler: create job log dir %s: %w", opts.JobLogDir, err)
	}

	uid := currentUIDFn()
	want := make(map[string]bool, len(jobs))

	for _, job := range jobs {
		want[job.Name] = true

		rendered, err := RenderPlist(job, opts.TriggerBinPath, opts.JobLogDir)
		if err != nil {
			return err
		}
		path := filepath.Join(opts.LaunchAgentsDir, plistFileName(job.Name))

		existing, readErr := os.ReadFile(path)
		if readErr == nil && string(existing) == rendered {
			continue // idempotent: content unchanged, nothing to do
		}

		if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
			return fmt.Errorf("scheduler: write plist %s: %w", path, err)
		}

		label := labelFor(job.Name)
		_ = runner.Run("bootout", "gui/"+uid+"/"+label) // ignore: "not loaded" is expected for a new/never-installed job
		if err := runner.Run("bootstrap", "gui/"+uid, path); err != nil {
			return fmt.Errorf("scheduler: bootstrap job %q: %w", job.Name, err)
		}
		if jsonl != nil {
			jsonl.Info("job_synced", "job_name", job.Name)
		}
	}

	entries, err := os.ReadDir(opts.LaunchAgentsDir)
	if err != nil {
		return fmt.Errorf("scheduler: read LaunchAgents dir %s: %w", opts.LaunchAgentsDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		jobName, ok := jobNameFromPlistFileName(e.Name())
		if !ok || want[jobName] {
			continue
		}
		label := labelFor(jobName)
		_ = runner.Run("bootout", "gui/"+uid+"/"+label) // ignore: best-effort, the file removal below is what actually matters
		if err := os.Remove(filepath.Join(opts.LaunchAgentsDir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("scheduler: remove stale plist for job %q: %w", jobName, err)
		}
		if jsonl != nil {
			jsonl.Info("job_removed", "job_name", jobName)
		}
	}
	return nil
}
