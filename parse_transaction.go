package cli

// This file implements the parse/commit boundary for a single Command.Run.
//
// Parsing a command graph mutates a lot of state: scalar values, slices and
// maps, user-supplied Destinations, IsSet/Count bookkeeping, value-source
// provenance and per-Command bookkeeping such as the set-flags map. A run
// that fails late (for example on the last argument, on an environment value
// or on required-flag validation) must not leave that state half applied.
//
// parseTransaction makes a run atomic from the caller's point of view:
//
//   - beginParse snapshots the observable state of every flag involved in the
//     run and resets the flag to its defaults, so every run parses from a
//     clean slate;
//   - while the run is in progress parsing behaves exactly as before and
//     writes straight into the usual holders and Destinations;
//   - finalize either commits (keeps the parsed state) or, when any parsing or
//     validation step failed, rolls every participant back to the snapshot
//     taken before the run.
//
// The hooks are deliberately optional: flags that only implement the public
// Flag interface keep their existing PreParse/Set/PostParse lifecycle, while
// the built-in flags additionally implement parseScopedFlag.

// parseScopedFlag is implemented by flags that can isolate the mutable state
// of one parse run and restore it when the run fails.
type parseScopedFlag interface {
	// beginParse snapshots the current observable flag state and resets the
	// flag to the state it has before a run (default value, not set).
	beginParse() error
	// commitParse is called once when the whole run succeeded; it releases
	// the snapshot. Values have already been written in place.
	commitParse()
	// rollbackParse is called when the run failed; it restores the snapshot
	// captured by beginParse.
	rollbackParse()
}

// restorableValue is implemented by Value holders whose Set mutates an
// in-place payload (interface-typed values such as TextFlag/GenericFlag) that
// swapping the holder alone cannot undo. The returned snapshot is opaque.
type restorableValue interface {
	snapshotValue() any
	restoreValue(any)
}

// parseStateArgument is implemented by positional Arguments that keep
// per-run mutable state beyond what is written to their Destination.
type parseStateArgument interface {
	saveParseState() any
	restoreParseState(any)
}

// cmdParseSnapshot captures the per-Command bookkeeping that parsing
// overwrites.
type cmdParseSnapshot struct {
	setFlags     map[Flag]struct{}
	appliedFlags []Flag
	parsedArgs   Args
	isInError    bool
}

// argParseSnapshot pairs a positional Argument with its opaque state.
type argParseSnapshot struct {
	arg   Argument
	state any
}

// parseTransaction tracks every participant of one Command.Run, starting at
// the root command and spanning the resolved sub-command chain.
type parseTransaction struct {
	// failed is set as soon as a parsing/validation step fails; finalize
	// then rolls back instead of committing.
	failed bool

	// began deduplicates flags across the command chain; persistent flags
	// are the same instance on ancestors and descendants.
	began map[Flag]struct{}
	// scoped keeps flags supporting rollback in begin order.
	scoped []parseScopedFlag

	// cmds snapshots the bookkeeping of every command frame entered.
	cmds map[*Command]*cmdParseSnapshot

	// args snapshots the positional arguments of the innermost command.
	args []argParseSnapshot
}

func newParseTransaction() *parseTransaction {
	return &parseTransaction{
		began: map[Flag]struct{}{},
		cmds:  map[*Command]*cmdParseSnapshot{},
	}
}

// markFailed records that the run hit a parsing or validation failure; the
// deferred finalize will roll back. Execution errors returned by user
// actions deliberately do not mark the transaction.
func (tx *parseTransaction) markFailed() {
	tx.failed = true
}

// snapshotCommand records the pre-run bookkeeping of a command frame. It is
// called once per Command in the chain.
func (tx *parseTransaction) snapshotCommand(cmd *Command) {
	if _, ok := tx.cmds[cmd]; ok {
		return
	}
	tx.cmds[cmd] = &cmdParseSnapshot{
		setFlags:     cmd.setFlags,
		appliedFlags: cmd.appliedFlags,
		parsedArgs:   cmd.parsedArgs,
		isInError:    cmd.isInError,
	}
}

// beginFlag starts the parse scope of one flag. A flag that implements
// parseScopedFlag gets a snapshot and a clean slate; any other flag goes
// through its existing PreParse lifecycle unchanged.
func (tx *parseTransaction) beginFlag(f Flag) error {
	if _, ok := tx.began[f]; ok {
		return nil
	}
	tx.began[f] = struct{}{}

	if sf, ok := f.(parseScopedFlag); ok {
		// Register before beginParse so a failing default validation still
		// restores the snapshot.
		tx.scoped = append(tx.scoped, sf)
		return sf.beginParse()
	}

	return f.PreParse()
}

// snapshotArguments records positional argument state before the innermost
// command parses its named Arguments.
func (tx *parseTransaction) snapshotArguments(args []Argument) {
	for _, a := range args {
		if sa, ok := a.(parseStateArgument); ok {
			tx.args = append(tx.args, argParseSnapshot{arg: a, state: sa.saveParseState()})
		}
	}
}

// finalize is deferred by the root command frame. It either publishes the run
// or restores the pre-run state of every participant.
func (tx *parseTransaction) finalize() {
	if tx.failed {
		tx.rollback()
		return
	}

	for _, sf := range tx.scoped {
		sf.commitParse()
	}
}

// rollback restores flags, positional arguments and command bookkeeping to
// their pre-run state. Flags are restored in reverse begin order.
func (tx *parseTransaction) rollback() {
	for i := len(tx.scoped) - 1; i >= 0; i-- {
		tx.scoped[i].rollbackParse()
	}

	for _, snap := range tx.args {
		if sa, ok := snap.arg.(parseStateArgument); ok {
			sa.restoreParseState(snap.state)
		}
	}

	for cmd, snap := range tx.cmds {
		cmd.setFlags = snap.setFlags
		cmd.appliedFlags = snap.appliedFlags
		cmd.parsedArgs = snap.parsedArgs
		cmd.isInError = snap.isInError
	}
}
