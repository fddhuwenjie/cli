package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the parse/commit boundary of a Command.Run:
//
// A failed run (bad command-line argument, unparsable environment/file
// source, inherited persistent flag, positional/required validation or flag
// post-processing) must leave every observable piece of state as it was
// before the call, and the next valid run must start from a clean slate.
// Successful runs commit all values at once with the existing precedence.

func quietCmd(t *testing.T, flags []Flag, action ActionFunc) *Command {
	t.Helper()
	if action == nil {
		action = func(context.Context, *Command) error { return nil }
	}
	return &Command{
		Name:      "app",
		Flags:     flags,
		Action:    action,
		Writer:    io.Discard,
		ErrWriter: io.Discard,
	}
}

// ---------------------------------------------------------------------------
// Late command-line argument errors
// ---------------------------------------------------------------------------

func TestParseBoundaryLateScalarError(t *testing.T) {
	var n int
	var s string
	cmd := quietCmd(t, []Flag{
		&IntFlag{Name: "n", Destination: &n},
		&StringFlag{Name: "s", Destination: &s},
	}, nil)

	// A valid flag is applied first; the error for the later flag must undo
	// the whole run instead of leaving partial state behind.
	err := cmd.Run(buildTestContext(t), []string{"app", "--n", "1", "--s", "ok", "--n", "bad"})
	require.ErrorContains(t, err, `invalid value "bad" for flag -n`)

	assert.Zero(t, n, "scalar destination after failed run")
	assert.Zero(t, s, "earlier scalar destination after failed run")
	assert.False(t, cmd.IsSet("n"))
	assert.False(t, cmd.IsSet("s"))
	assert.Zero(t, cmd.NumFlags())
	assert.Zero(t, cmd.Count("n"))
}

func TestParseBoundaryUnknownFlagAndMissingArgument(t *testing.T) {
	var n int
	mkCmd := func() (*Command, *int) {
		cmd := quietCmd(t, []Flag{
			&IntFlag{Name: "n", Destination: &n},
		}, nil)
		return cmd, &n
	}

	cmd, _ := mkCmd()
	err := cmd.Run(buildTestContext(t), []string{"app", "--n", "1", "--unknown"})
	require.ErrorContains(t, err, "flag provided but not defined")
	require.ErrorContains(t, err, "-unknown")
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))
	assert.Zero(t, cmd.NumFlags())

	n = 0
	cmd2, _ := mkCmd()
	err = cmd2.Run(buildTestContext(t), []string{"app", "--n"})
	require.ErrorContains(t, err, "flag needs an argument: --n")
	assert.Zero(t, n)
	assert.False(t, cmd2.IsSet("n"))
}

func TestParseBoundaryOriginalErrorOnUsageError(t *testing.T) {
	var seen error
	cmd := quietCmd(t, []Flag{&IntFlag{Name: "n"}}, nil)
	cmd.OnUsageError = func(_ context.Context, _ *Command, err error, _ bool) error {
		seen = err
		return err
	}

	err := cmd.Run(buildTestContext(t), []string{"app", "--n", "bad"})
	require.Error(t, err)
	require.ErrorContains(t, seen, `invalid value "bad" for flag -n`)
	assert.False(t, cmd.IsSet("n"))
}

// ---------------------------------------------------------------------------
// Environment / file source parsing errors
// ---------------------------------------------------------------------------

func TestParseBoundaryEnvSourceError(t *testing.T) {
	t.Setenv("APP_N", "not-an-int")

	var n int
	fl := &IntFlag{Name: "n", Destination: &n, Sources: EnvVars("APP_N")}
	cmd := quietCmd(t, []Flag{fl}, nil)

	err := cmd.Run(buildTestContext(t), []string{"app"})
	require.Error(t, err)
	assert.ErrorContains(t, err, `could not parse "not-an-int" as int value`)
	assert.ErrorContains(t, err, "environment variable")
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))
	assert.Zero(t, cmd.NumFlags())

	// The next valid run starts from a clean slate: CLI value applies.
	err = cmd.Run(buildTestContext(t), []string{"app", "--n", "42"})
	require.NoError(t, err)
	assert.Equal(t, 42, n)
	assert.True(t, cmd.IsSet("n"))
	assert.Equal(t, 1, cmd.Count("n"))
}

func TestParseBoundaryFileSourceError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "n.txt")
	require.NoError(t, os.WriteFile(path, []byte("oops"), 0o600))

	var n int
	cmd := quietCmd(t, []Flag{
		&IntFlag{Name: "n", Destination: &n, Sources: Files(path)},
	}, nil)

	err := cmd.Run(buildTestContext(t), []string{"app"})
	require.ErrorContains(t, err, "could not parse")
	assert.ErrorContains(t, err, filepath.Clean(path))
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))

	require.NoError(t, os.WriteFile(path, []byte("7"), 0o600))
	err = cmd.Run(buildTestContext(t), []string{"app"})
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	assert.True(t, cmd.IsSet("n"))
}

// ---------------------------------------------------------------------------
// Parent / child commands and persistent flags
// ---------------------------------------------------------------------------

func TestParseBoundaryParentChildFailureRollsBackChain(t *testing.T) {
	var p, c int
	root := &Command{
		Name: "app", Writer: io.Discard, ErrWriter: io.Discard,
		Flags: []Flag{&IntFlag{Name: "p", Destination: &p}},
		Commands: []*Command{{
			Name:   "sub",
			Action: func(context.Context, *Command) error { return nil },
			Flags:  []Flag{&IntFlag{Name: "c", Destination: &c}},
		}},
	}

	err := root.Run(buildTestContext(t), []string{"app", "sub", "--p", "1", "--c", "bad"})
	require.ErrorContains(t, err, `invalid value "bad" for flag -c`)

	// Both the child flag and the ancestor persistent flag are rolled back,
	// on the Destinations as well as through IsSet/NumFlags.
	assert.Zero(t, p)
	assert.Zero(t, c)
	sub := root.Command("sub")
	assert.False(t, root.IsSet("p"))
	assert.False(t, sub.IsSet("c"))
	assert.False(t, sub.IsSet("p"))
	assert.Zero(t, root.NumFlags())
	assert.Zero(t, sub.NumFlags())

	// Next valid run of the same command graph starts clean.
	err = root.Run(buildTestContext(t), []string{"app", "sub", "--p", "9", "--c", "2"})
	require.NoError(t, err)
	assert.Equal(t, 9, p)
	assert.Equal(t, 2, c)
	assert.True(t, sub.IsSet("p"))
	assert.True(t, sub.IsSet("c"))
}

func TestParseBoundaryParentChildRestoresCommittedChain(t *testing.T) {
	var p, c int
	root := &Command{
		Name: "app", Writer: io.Discard, ErrWriter: io.Discard,
		Flags: []Flag{&IntFlag{Name: "p", Destination: &p}},
		Commands: []*Command{{
			Name:   "sub",
			Action: func(context.Context, *Command) error { return nil },
			Flags:  []Flag{&IntFlag{Name: "c", Destination: &c}},
		}},
	}

	// First run commits p=1, c=2.
	require.NoError(t, root.Run(buildTestContext(t), []string{"app", "sub", "--p", "1", "--c", "2"}))
	require.Equal(t, 1, p)
	require.Equal(t, 2, c)

	// Second run fails in the child: the pre-call, committed chain state is
	// observable afterwards.
	err := root.Run(buildTestContext(t), []string{"app", "sub", "--c", "bad"})
	require.Error(t, err)
	assert.Equal(t, 1, p, "committed parent value preserved")
	assert.Equal(t, 2, c, "committed child value preserved")
	sub := root.Command("sub")
	assert.True(t, sub.IsSet("p"), "committed IsSet state preserved")
	assert.True(t, sub.IsSet("c"))

	// A valid run without --p parses from defaults, not from the committed
	// value of an earlier run.
	require.NoError(t, root.Run(buildTestContext(t), []string{"app", "sub", "--c", "3"}))
	assert.Zero(t, p, "fresh run does not inherit stale persistent value")
	assert.Equal(t, 3, c)
}

func TestParseBoundaryChildDoesNotLeakIntoParent(t *testing.T) {
	var parentLocal, childLocal int
	root := &Command{
		Name: "app", Writer: io.Discard, ErrWriter: io.Discard,
		Action: func(context.Context, *Command) error { return nil },
		Flags:  []Flag{&IntFlag{Name: "parent", Destination: &parentLocal}},
		Commands: []*Command{{
			Name:   "sub",
			Action: func(context.Context, *Command) error { return nil },
			Flags:  []Flag{&IntFlag{Name: "child", Destination: &childLocal, Local: true}},
		}},
	}

	require.NoError(t, root.Run(buildTestContext(t), []string{"app", "sub", "--child", "5"}))
	assert.Equal(t, 5, childLocal)
	assert.Zero(t, parentLocal)
	assert.False(t, root.IsSet("parent"))
	sub := root.Command("sub")
	assert.False(t, sub.IsSet("parent"), "child must not see the parent's local flag as set")
}

// ---------------------------------------------------------------------------
// Positional and required validation
// ---------------------------------------------------------------------------

func TestParseBoundaryRequiredFlagRollsBackOptional(t *testing.T) {
	var opt string
	cmd := quietCmd(t, []Flag{
		&StringFlag{Name: "req", Required: true},
		&StringFlag{Name: "opt", Destination: &opt},
	}, nil)

	err := cmd.Run(buildTestContext(t), []string{"app", "--opt", "value"})
	require.Error(t, err)
	assert.ErrorContains(t, err, `Required flag "req" not set`)
	assert.Empty(t, opt)
	assert.False(t, cmd.IsSet("opt"))
	assert.Zero(t, cmd.NumFlags())

	// Next valid run works.
	err = cmd.Run(buildTestContext(t), []string{"app", "--req", "r", "--opt", "o"})
	require.NoError(t, err)
	assert.Equal(t, "o", opt)
	assert.True(t, cmd.IsSet("opt"))
}

func TestParseBoundaryPositionalArgumentFailure(t *testing.T) {
	var a, b int
	cmd := quietCmd(t, nil, nil)
	cmd.Arguments = []Argument{
		&IntArg{Name: "a", Destination: &a},
		&IntArg{Name: "b", Destination: &b},
	}

	// The first argument parses, the second fails: the first Destination and
	// the accessor state must roll back.
	err := cmd.Run(buildTestContext(t), []string{"app", "1", "bad"})
	require.ErrorContains(t, err, `invalid value "bad" for argument b`)
	assert.Zero(t, a)
	assert.Zero(t, b)
	assert.Zero(t, cmd.IntArg("a"))
	assert.Zero(t, cmd.IntArg("b"))

	err = cmd.Run(buildTestContext(t), []string{"app", "4", "5"})
	require.NoError(t, err)
	assert.Equal(t, 4, a)
	assert.Equal(t, 5, b)
	assert.Equal(t, 4, cmd.IntArg("a"))
}

func TestParseBoundaryRequiredArgument(t *testing.T) {
	var a string
	cmd := quietCmd(t, nil, nil)
	cmd.Arguments = []Argument{
		&StringArg{Name: "a", Required: true, Destination: &a},
	}

	err := cmd.Run(buildTestContext(t), []string{"app"})
	require.Error(t, err)
	assert.ErrorContains(t, err, `Required argument "a" not set`)
	assert.Empty(t, a)

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "x"}))
	assert.Equal(t, "x", a)
}

func TestParseBoundaryArgValidatorFailure(t *testing.T) {
	var n int
	cmd := quietCmd(t, []Flag{&IntFlag{Name: "n", Destination: &n}}, nil)
	cmd.ArgValidator = func(_ context.Context, c *Command) error {
		return errors.New("bad positional arguments")
	}

	err := cmd.Run(buildTestContext(t), []string{"app", "--n", "1", "leftover"})
	require.ErrorContains(t, err, "bad positional arguments")
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))
}

// ---------------------------------------------------------------------------
// Custom flags
// ---------------------------------------------------------------------------

// lifecycleFlag only implements the public Flag interface; the library must
// keep its PreParse/Set/PostParse lifecycle and original errors untouched.
type lifecycleFlag struct {
	name      string
	value     string
	preParse  int
	postParse int
	sets      []string
	failOn    string
}

func (f *lifecycleFlag) String() string   { return f.name }
func (f *lifecycleFlag) Names() []string  { return []string{f.name} }
func (f *lifecycleFlag) Get() any         { return f.value }
func (f *lifecycleFlag) IsSet() bool      { return f.value != "" }
func (f *lifecycleFlag) PreParse() error  { f.preParse++; f.value = ""; return nil }
func (f *lifecycleFlag) PostParse() error { f.postParse++; return nil }
func (f *lifecycleFlag) Set(_ string, v string) error {
	if v == f.failOn {
		return errors.New("custom boom")
	}
	f.sets = append(f.sets, v)
	f.value = v
	return nil
}

func TestParseBoundaryCustomFlagLifecycleAndError(t *testing.T) {
	fl := &lifecycleFlag{name: "custom", failOn: "bad"}
	cmd := quietCmd(t, []Flag{fl}, nil)

	err := cmd.Run(buildTestContext(t), []string{"app", "--custom", "bad"})
	require.ErrorContains(t, err, `invalid value "bad" for flag -custom: custom boom`)
	assert.Equal(t, 1, fl.preParse)
	assert.Equal(t, 0, fl.postParse, "PostParse is not reached on parse failure")

	// The custom flag resets itself through its own PreParse lifecycle, so
	// the next valid run works without any special support.
	err = cmd.Run(buildTestContext(t), []string{"app", "--custom", "good"})
	require.NoError(t, err)
	assert.Equal(t, 2, fl.preParse)
	assert.Equal(t, 1, fl.postParse)
	assert.Equal(t, "good", fl.value)
	assert.Equal(t, []string{"good"}, fl.sets, "failed set value is not carried into the next run")
}

// scopedCustomFlag additionally implements the internal parse scope hooks to
// opt into atomic rollback.
type scopedCustomFlag struct {
	lifecycleFlag

	began, committed, rolledBack int
}

func (f *scopedCustomFlag) beginParse() error {
	f.began++
	return f.PreParse()
}

func (f *scopedCustomFlag) commitParse() {
	f.committed++
}

func (f *scopedCustomFlag) rollbackParse() {
	f.rolledBack++
	f.value = ""
}

func TestParseBoundaryScopedCustomFlag(t *testing.T) {
	fl := &scopedCustomFlag{lifecycleFlag: lifecycleFlag{name: "custom", failOn: "bad"}}
	cmd := quietCmd(t, []Flag{fl}, nil)

	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--custom", "bad"}))
	assert.Equal(t, 1, fl.began)
	assert.Zero(t, fl.committed)
	assert.Equal(t, 1, fl.rolledBack)
	assert.Empty(t, fl.Get())

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--custom", "good"}))
	assert.Equal(t, 2, fl.began)
	assert.Equal(t, 1, fl.committed)
	assert.Equal(t, 1, fl.rolledBack)
	assert.Equal(t, "good", fl.Get())
}

// ---------------------------------------------------------------------------
// Repeated calls
// ---------------------------------------------------------------------------

func TestParseBoundaryRepeatedRuns(t *testing.T) {
	var n int
	var s string
	cmd := quietCmd(t, []Flag{
		&IntFlag{Name: "n", Destination: &n},
		&StringFlag{Name: "s", Destination: &s},
	}, nil)

	// failed -> success -> failed -> success
	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--n", "bad"}))
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--n", "1", "--s", "a"}))
	assert.Equal(t, 1, n)
	assert.Equal(t, "a", s)
	assert.Equal(t, 1, cmd.Count("n"))
	assert.Equal(t, 2, cmd.NumFlags())

	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--n", "2", "--s", "b", "--n", "nope"}))
	assert.Equal(t, 1, n, "committed value restored after failed run")
	assert.Equal(t, "a", s)
	assert.True(t, cmd.IsSet("n"))
	assert.Equal(t, 1, cmd.Count("n"), "per-run count restored to committed state")

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--n", "3"}))
	assert.Equal(t, 3, n)
	assert.Empty(t, s, "clean run resets flags not provided")
	assert.Equal(t, 1, cmd.NumFlags())
	assert.Equal(t, 1, cmd.Count("n"))
}

func TestParseBoundaryRepeatedSuccessIsDeterministic(t *testing.T) {
	var n int
	cmd := quietCmd(t, []Flag{&IntFlag{Name: "n", Destination: &n}}, nil)

	for i := 1; i <= 3; i++ {
		require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--n", fmt.Sprint(i)}))
		assert.Equal(t, i, n)
		assert.Equal(t, 1, cmd.Count("n"), "count must not accumulate across runs")
		assert.Equal(t, 1, cmd.NumFlags())
	}

	// A run without the flag resets to the default instead of reusing it.
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app"}))
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))
	assert.Zero(t, cmd.NumFlags())
}

// ---------------------------------------------------------------------------
// Slices and maps
// ---------------------------------------------------------------------------

func TestParseBoundarySlicePartialAppend(t *testing.T) {
	var ns []int
	var ss []string
	cmd := quietCmd(t, []Flag{
		&IntSliceFlag{Name: "ns", Destination: &ns},
		&StringSliceFlag{Name: "ss", Destination: &ss},
	}, nil)

	// Earlier repeated occurrences parse; a later one fails.
	err := cmd.Run(buildTestContext(t), []string{"app", "--ns", "1", "--ss", "a", "--ns", "2", "--ns", "bad"})
	require.ErrorContains(t, err, `invalid value "bad" for flag -ns`)
	assert.Nil(t, ns)
	assert.Nil(t, ss)
	assert.False(t, cmd.IsSet("ns"))
	assert.False(t, cmd.IsSet("ss"))
	assert.Zero(t, cmd.NumFlags())

	// A bad element inside a comma-separated token poisons the whole run.
	err = cmd.Run(buildTestContext(t), []string{"app", "--ns", "1,2,bad"})
	require.Error(t, err)
	assert.Nil(t, ns)
	assert.False(t, cmd.IsSet("ns"))

	// Next valid run commits exactly its own values.
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--ns", "8", "--ns", "9", "--ss", "z"}))
	assert.Equal(t, []int{8, 9}, ns)
	assert.Equal(t, []string{"z"}, ss)
	assert.Equal(t, 2, cmd.Count("ns"), "occurrences count per successful run")
	assert.Equal(t, 1, cmd.Count("ss"))

	// After a commit, another failed run restores the committed slices.
	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--ns", "1", "--ns", "bad"}))
	assert.Equal(t, []int{8, 9}, ns, "committed slice restored")
	assert.Equal(t, []string{"z"}, ss, "untouched flag keeps committed slice")
	assert.True(t, cmd.IsSet("ns"))

	// The next valid run commits exactly its own values again.
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--ns", "3"}))
	assert.Equal(t, []int{3}, ns)
	assert.Empty(t, ss, "flag not provided resets to its empty default")
}

func TestParseBoundarySliceDefaultRestored(t *testing.T) {
	var ns []int
	fl := &IntSliceFlag{Name: "ns", Destination: &ns, Value: []int{1, 2}}
	cmd := quietCmd(t, []Flag{fl}, nil)

	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--ns", "bad"}))
	assert.Nil(t, ns, "destination untouched by failed run")
	assert.Equal(t, []int{1, 2}, cmd.Value("ns"), "declared default remains observable")
	assert.False(t, cmd.IsSet("ns"))

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--ns", "3"}))
	assert.Equal(t, []int{3}, ns)
	assert.True(t, cmd.IsSet("ns"))
}

func TestParseBoundaryMapPartialEntry(t *testing.T) {
	var mm map[string]string
	cmd := quietCmd(t, []Flag{
		&StringMapFlag{Name: "mm", Destination: &mm},
		&IntFlag{Name: "after", Value: 0},
	}, nil)

	// A valid entry is followed by an item without a key/value separator.
	err := cmd.Run(buildTestContext(t), []string{"app", "--mm", "a=1", "--mm", "no-separator"})
	require.ErrorContains(t, err, `item "no-separator" is missing separator`)
	assert.Nil(t, mm)
	assert.False(t, cmd.IsSet("mm"))

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--mm", "a=1", "--mm", "b=2"}))
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, mm)
	assert.True(t, cmd.IsSet("mm"))

	// A later failed run restores the committed map.
	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--mm", "c=3", "--mm", "bad-entry"}))
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, mm)
}

func TestParseBoundaryBoolWithInverseBothSet(t *testing.T) {
	var env bool
	fl := &BoolWithInverseFlag{Name: "env", Aliases: []string{"e"}, Destination: &env}
	cmd := quietCmd(t, []Flag{fl}, nil)

	err := cmd.Run(buildTestContext(t), []string{"app", "--env", "--no-env"})
	require.ErrorContains(t, err, "cannot set both flags")
	assert.False(t, env)
	assert.False(t, cmd.IsSet("env"))
	assert.Zero(t, cmd.Count("env"))

	// Internal positive/negative tracking is reset as well, so either form
	// works on the next run.
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--no-env"}))
	assert.False(t, env)
	assert.True(t, cmd.IsSet("env"))
	assert.Equal(t, 1, cmd.Count("env"))
}

func TestParseBoundaryMutuallyExclusiveRollback(t *testing.T) {
	var a, b string
	cmd := quietCmd(t, nil, nil)
	cmd.MutuallyExclusiveFlags = []MutuallyExclusiveFlags{{
		Flags: [][]Flag{
			{&StringFlag{Name: "a", Destination: &a}},
			{&StringFlag{Name: "b", Destination: &b}},
		},
	}}

	err := cmd.Run(buildTestContext(t), []string{"app", "--a", "1", "--b", "2"})
	require.Error(t, err)
	assert.Empty(t, a)
	assert.Empty(t, b)
	assert.False(t, cmd.IsSet("a"))
	assert.False(t, cmd.IsSet("b"))
	assert.Zero(t, cmd.NumFlags())

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--a", "1"}))
	assert.Equal(t, "1", a)
}

// ---------------------------------------------------------------------------
// Successful run precedence and one-shot commit
// ---------------------------------------------------------------------------

func TestParseBoundarySuccessPrecedence(t *testing.T) {
	var n int
	cmd := quietCmd(t, []Flag{
		&IntFlag{Name: "n", Destination: &n, Value: 3},
	}, nil)

	// default only
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app"}))
	assert.Equal(t, 3, n)
	assert.False(t, cmd.IsSet("n"))
}

func TestParseBoundarySuccessEnvAndCliPrecedence(t *testing.T) {
	t.Setenv("APP_N", "5")

	var n int
	fl := &IntFlag{Name: "n", Destination: &n, Value: 3, Sources: EnvVars("APP_N")}
	cmd := quietCmd(t, []Flag{fl}, nil)

	// env beats default
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app"}))
	assert.Equal(t, 5, n)
	assert.True(t, cmd.IsSet("n"))
	assert.Equal(t, 1, cmd.Count("n"))

	// CLI beats env and does not accumulate with it
	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--n", "7"}))
	assert.Equal(t, 7, n)
	assert.Equal(t, 1, cmd.Count("n"))
}

// ---------------------------------------------------------------------------
// Help output behavior
// ---------------------------------------------------------------------------

func TestParseBoundaryHelpKeepsSuccessfulParse(t *testing.T) {
	var n int
	cmd := quietCmd(t, []Flag{&IntFlag{Name: "n", Destination: &n}}, nil)

	// A syntactically valid run ending in --help succeeds (returns nil) and
	// keeps the parsed values, just like before the transaction boundary.
	err := cmd.Run(buildTestContext(t), []string{"app", "--n", "1", "--help"})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, cmd.IsSet("n"))
}

func TestParseBoundaryFailedParseWithHelpRollsBack(t *testing.T) {
	var n int
	cmd := quietCmd(t, []Flag{
		&IntFlag{Name: "n", Destination: &n},
	}, nil)

	// --help is parsed first, so when parsing --n fails afterwards the run
	// shows help and returns nil; the failed parse must still be rolled back.
	err := cmd.Run(buildTestContext(t), []string{"app", "--help", "--n", "bad"})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.False(t, cmd.IsSet("n"))
}

// ---------------------------------------------------------------------------
// Interface-typed flags mutate their payload in place
// ---------------------------------------------------------------------------

func TestParseBoundaryTextFlagInPlaceRestore(t *testing.T) {
	var addrVal netip.Addr
	var addr TextMarshalUnmarshaler = &addrVal
	cmd := quietCmd(t, []Flag{
		&TextFlag{Name: "addr", Destination: &addr},
	}, nil)

	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--addr", "not-an-ip"}))
	assert.False(t, addrVal.IsValid(), "failed run leaves destination at zero value")

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--addr", "10.0.0.1"}))
	first := netip.MustParseAddr("10.0.0.1")
	assert.Equal(t, first, addrVal)

	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--addr", "bad"}))
	assert.Equal(t, first, addrVal, "failed run restores committed value in place")
}

// serializableValue is a GenericFlag payload supporting Serializer, so its
// in-place mutation is reversible through re-ingestion.
type serializableValue struct{ text string }

func (v *serializableValue) String() string { return v.text }
func (v *serializableValue) Get() any       { return v.text }
func (v *serializableValue) Set(s string) error {
	if s == "bad" {
		return errors.New("bad generic value")
	}
	v.text = s
	return nil
}
func (v *serializableValue) Serialize() string { return v.text }

func TestParseBoundaryGenericFlagSerializerRestore(t *testing.T) {
	payload := &serializableValue{text: "committed"}
	cmd := quietCmd(t, []Flag{
		&GenericFlag{Name: "g", Value: payload},
	}, nil)

	require.NoError(t, cmd.Run(buildTestContext(t), []string{"app", "--g", "committed"}))
	require.Equal(t, "committed", payload.text)

	require.Error(t, cmd.Run(buildTestContext(t), []string{"app", "--g", "bad"}))
	assert.Equal(t, "committed", payload.text, "failed generic parse restores serialized content")
}

// ---------------------------------------------------------------------------
// Concurrency: independent Commands must not cross-write state
// ---------------------------------------------------------------------------

func TestParseBoundaryIndependentCommandsConcurrent(t *testing.T) {
	const goroutines = 16
	const iterations = 40

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()

			// Every goroutine owns an independent Command graph with its own
			// flag instances and Destination.
			var n int
			cmd := quietCmd(t, []Flag{&IntFlag{Name: "n", Destination: &n}}, nil)

			// A failed run restores the last committed value (zero until the
			// first successful run).
			lastCommitted := 0
			for i := 0; i < iterations; i++ {
				args := []string{"app"}
				wantFail := (g+i)%3 == 0
				if wantFail {
					args = append(args, "--n", "bad")
					if err := cmd.Run(context.Background(), args); err == nil {
						errCh <- fmt.Errorf("g%d i%d: expected error", g, i)
						return
					}
					if n != lastCommitted {
						errCh <- fmt.Errorf("g%d i%d: rolled back destination = %d, want %d", g, i, n, lastCommitted)
						return
					}
					continue
				}

				v := g*iterations + i
				args = append(args, "--n", fmt.Sprint(v))
				if err := cmd.Run(context.Background(), args); err != nil {
					errCh <- fmt.Errorf("g%d i%d: %w", g, i, err)
					return
				}
				if n != v {
					errCh <- fmt.Errorf("g%d i%d: got %d want %d", g, i, n, v)
					return
				}
				lastCommitted = v
				if cmd.Count("n") != 1 || cmd.NumFlags() != 1 {
					errCh <- fmt.Errorf("g%d i%d: stale per-run state count=%d num=%d", g, i, cmd.Count("n"), cmd.NumFlags())
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		assert.NoError(t, err)
	}
}
