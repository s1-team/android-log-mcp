package filter

// Countable reports whether n can be evaluated exactly from per-block class
// counts (issue #37): every term is on tag / package / process / level, or a
// ClassOnly Pred (package= / tags). age: at the top level is a time window
// and is stripped before this runs; age: inside OR is not countable.
func Countable(n Node) bool {
	switch x := n.(type) {
	case Empty:
		return true
	case *And:
		for _, k := range x.Kids {
			if !Countable(k) {
				return false
			}
		}
		return true
	case *Or:
		for _, k := range x.Kids {
			if !Countable(k) {
				return false
			}
		}
		return true
	case *Str:
		return classField(x.Field)
	case *Regex:
		return classField(x.Field)
	case *Level:
		return true
	case *Pred:
		return x.ClassOnly
	default:
		// Age, Crash, StackTrace, Num, Buffer, message/line fields, …
		return false
	}
}

func classField(f Field) bool {
	return f == FTag || f == FPackage || f == FProcess
}
