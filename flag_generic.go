package cli

type GenericFlag = FlagBase[Value, NoConfig, genericValue]

// -- Value Value
type genericValue struct {
	val Value
}

// Below functions are to satisfy the ValueCreator interface

func (f genericValue) Create(val Value, p *Value, c NoConfig) Value {
	*p = val
	return &genericValue{
		val: *p,
	}
}

func (f genericValue) ToString(b Value) string {
	f.val = b
	return f.String()
}

// Below functions are to satisfy the flag.Value interface

func (f *genericValue) Set(s string) error {
	if f.val != nil {
		return f.val.Set(s)
	}
	return nil
}

func (f *genericValue) Get() any {
	if f.val != nil {
		return f.val.Get()
	}
	return nil
}

func (f *genericValue) String() string {
	if f.val != nil {
		return f.val.String()
	}
	return ""
}

func (f *genericValue) IsBoolFlag() bool {
	if f.val == nil {
		return false
	}
	bf, ok := f.val.(boolFlag)
	return ok && bf.IsBoolFlag()
}

// snapshotValue captures the serialized form of the wrapped value when it
// can be re-ingested through Set; values without Serializer cannot be
// restored in place.
func (f *genericValue) snapshotValue() any {
	if f.val == nil {
		return nil
	}
	if s, ok := f.val.(Serializer); ok {
		return s.Serialize()
	}
	return nil
}

// restoreValue re-ingests the captured serialized content.
func (f *genericValue) restoreValue(snap any) {
	if f.val == nil {
		return
	}
	if raw, ok := snap.(string); ok {
		_ = f.val.Set(raw)
	}
}

// Generic looks up the value of a local GenericFlag, returns
// nil if not found
func (cmd *Command) Generic(name string) Value {
	if fl := cmd.lookupFlag(name); fl != nil {
		if v, ok := fl.Get().(Value); ok {
			tracef("generic available for flag name %[1]q with value=%[2]v (cmd=%[3]q)", name, v, cmd.Name)
			return v
		}
		if fl, ok := fl.(*GenericFlag); ok {
			if value, ok := fl.value.(*genericValue); ok {
				return value.val
			}
		}
	}

	tracef("generic NOT available for flag name %[1]q (cmd=%[2]q)", name, cmd.Name)
	return nil
}
