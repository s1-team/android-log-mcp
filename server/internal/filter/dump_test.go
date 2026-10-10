package filter

import (
	"fmt"
	"strings"
)

// dump renders a node structurally, close to the AS Kotlin data classes.
func dump(n Node) string {
	switch x := n.(type) {
	case nil:
		return "null"
	case *And:
		return "And(" + dumpKids(x.Kids) + ")"
	case *Or:
		return "Or(" + dumpKids(x.Kids) + ")"
	case Empty:
		return "Empty"
	case *Str:
		f := strings.ToUpper(x.Field.String())
		if x.Field == FImplicitLine {
			f = "IMPLICIT_LINE"
		}
		if x.Field == FPackage {
			f = "APP"
		}
		kind := "String"
		if x.Exact {
			kind = "ExactString"
		}
		if x.Neg {
			kind = "Negated" + kind
		}
		return fmt.Sprintf("%s(%q,%s)", kind, x.Value, f)
	case *Regex:
		f := strings.ToUpper(x.Field.String())
		if x.Field == FPackage {
			f = "APP"
		}
		k := "Regex"
		if x.Neg {
			k = "NegatedRegex"
		}
		return fmt.Sprintf("%s(%q,%s)", k, x.Src, f)
	case *Level:
		return "Level(" + x.Min.Name() + ")"
	case *Age:
		return "Age(" + x.Src + ")"
	case Crash:
		return "Crash"
	case StackTrace:
		return "StackTrace"
	case *Num:
		return fmt.Sprintf("Num(%s,%d,%v)", x.Key, x.N, x.Neg)
	case *Buffer:
		return fmt.Sprintf("Buffer(%s,%v)", x.Name, x.Neg)
	}
	return fmt.Sprintf("%T", n)
}

func dumpKids(k []Node) string {
	s := make([]string, len(k))
	for i, n := range k {
		s[i] = dump(n)
	}
	return strings.Join(s, ", ")
}
