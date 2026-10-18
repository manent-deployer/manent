package policy

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/tester"
	"github.com/open-policy-agent/opa/v1/types"

	"github.com/joeg-ita/manent/internal/glob"
)

// libFS holds the Rego library of manent (packages manent.lib, manent.lib.rules) and the
// default policy.
//
//go:embed rego/*.rego
var libFS embed.FS

// libModules are the library modules, added to every revision.
var libModules = []string{"rego/lib.rego", "rego/rules.rego"}

// defaultModule is the policy used when a revision defines no manent.governance package.
const defaultModule = "rego/default.rego"

// reservedPackage is the prefix of the library packages: revisions cannot define them.
const reservedPackage = "data.manent.lib"

// builtin is a Go function made available to the policies.
type builtin struct {
	decl *rego.Function
	opt  func(*rego.Rego)
}

// builtins are the deterministic functions of the library.
var builtins = []builtin{
	newBuiltin2("manent.glob_any", types.Args(types.NewArray(nil, types.S), types.S), types.B,
		func(patterns, value *ast.Term) (*ast.Term, error) {
			var ps []string
			if err := ast.As(patterns.Value, &ps); err != nil {
				return nil, errors.New("manent.glob_any: patterns must be an array of strings")
			}
			s, ok := value.Value.(ast.String)
			if !ok {
				return nil, errors.New("manent.glob_any: value must be a string")
			}
			return ast.BooleanTerm(glob.Any(ps, string(s))), nil
		}),
	newBuiltin2("manent.semver_less", types.Args(types.S, types.S), types.B,
		func(a, b *ast.Term) (*ast.Term, error) {
			x, ok1 := a.Value.(ast.String)
			y, ok2 := b.Value.(ast.String)
			if !ok1 || !ok2 {
				return nil, errors.New("manent.semver_less: versions must be strings")
			}
			return ast.BooleanTerm(compareVersions(string(x), string(y)) < 0), nil
		}),
	newBuiltin2("manent.local_time", types.Args(types.S, types.S), types.NewObject(nil, types.NewDynamicProperty(types.S, types.N)),
		func(t, tz *ast.Term) (*ast.Term, error) {
			s, ok1 := t.Value.(ast.String)
			z, ok2 := tz.Value.(ast.String)
			if !ok1 || !ok2 {
				return nil, errors.New("manent.local_time: instant and time zone must be strings")
			}
			at, err := time.Parse(time.RFC3339Nano, string(s))
			if err != nil {
				return nil, fmt.Errorf("manent.local_time: %w", err)
			}
			loc, err := time.LoadLocation(string(z))
			if err != nil {
				return nil, fmt.Errorf("manent.local_time: %w", err)
			}
			lt := at.In(loc)
			return ast.ObjectTerm(
				ast.Item(ast.StringTerm("year"), ast.IntNumberTerm(lt.Year())),
				ast.Item(ast.StringTerm("month"), ast.IntNumberTerm(int(lt.Month()))),
				ast.Item(ast.StringTerm("day"), ast.IntNumberTerm(lt.Day())),
				ast.Item(ast.StringTerm("weekday"), ast.IntNumberTerm(int(lt.Weekday()))),
				ast.Item(ast.StringTerm("hour"), ast.IntNumberTerm(lt.Hour())),
				ast.Item(ast.StringTerm("minute"), ast.IntNumberTerm(lt.Minute())),
			), nil
		}),
	newBuiltin1("manent.duration_ns", types.Args(types.S), types.N,
		func(d *ast.Term) (*ast.Term, error) {
			s, ok := d.Value.(ast.String)
			if !ok {
				return nil, errors.New("manent.duration_ns: the duration must be a string")
			}
			v, err := time.ParseDuration(string(s))
			if err != nil {
				return nil, fmt.Errorf("manent.duration_ns: %w", err)
			}
			return ast.NumberTerm(json.Number(strconv.FormatInt(int64(v), 10))), nil
		}),
}

func newBuiltin1(name string, args []types.Type, result types.Type, f func(*ast.Term) (*ast.Term, error)) builtin {
	decl := &rego.Function{Name: name, Decl: types.NewFunction(args, result)}
	return builtin{decl: decl, opt: rego.Function1(decl, func(_ rego.BuiltinContext, a *ast.Term) (*ast.Term, error) { return f(a) })}
}

func newBuiltin2(name string, args []types.Type, result types.Type, f func(a, b *ast.Term) (*ast.Term, error)) builtin {
	decl := &rego.Function{Name: name, Decl: types.NewFunction(args, result)}
	return builtin{decl: decl, opt: rego.Function2(decl, func(_ rego.BuiltinContext, a, b *ast.Term) (*ast.Term, error) { return f(a, b) })}
}

// forbiddenBuiltins are the built-ins with side effects or reading the clock, the network or
// the runtime, besides those OPA marks as nondeterministic: an evaluation must depend only on
// input and data, so the same input with the same revision always gives the same decision.
var forbiddenBuiltins = []string{
	"trace", "providers.aws.sign_req",
	"crypto.x509.parse_and_verify_certificates", "crypto.x509.parse_and_verify_certificates_with_options",
}

// capabilities are those of this OPA version without the forbidden built-ins, plus the
// library's.
func capabilities() *ast.Capabilities {
	// The returned capabilities may be shared: they are copied before changing them.
	c := *ast.CapabilitiesForThisVersion()
	caps := &c
	caps.Builtins = slices.DeleteFunc(slices.Clone(caps.Builtins), func(b *ast.Builtin) bool {
		return b.Nondeterministic || slices.Contains(forbiddenBuiltins, b.Name)
	})
	for _, b := range builtins {
		caps.Builtins = append(caps.Builtins, &ast.Builtin{Name: b.decl.Name, Decl: b.decl.Decl})
	}
	caps.AllowNet = []string{}
	return caps
}

// testBuiltins are the library functions for the test runner.
func testBuiltins() []*tester.Builtin {
	out := make([]*tester.Builtin, len(builtins))
	for i, b := range builtins {
		out[i] = &tester.Builtin{Decl: &ast.Builtin{Name: b.decl.Name, Decl: b.decl.Decl}, Func: b.opt}
	}
	return out
}

// compareVersions compares two versions: -1, 0 or 1.
func compareVersions(a, b string) int {
	a, b = strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")
	ra, pa, _ := strings.Cut(a, "-")
	rb, pb, _ := strings.Cut(b, "-")
	if c := compareDotted(ra, rb); c != 0 {
		return c
	}
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	return compareDotted(pa, pb)
}

// compareDotted compares dot-separated parts: numbers as numbers, otherwise as strings.
func compareDotted(a, b string) int {
	xs, ys := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(xs), len(ys)) {
		x, y := "0", "0"
		if i < len(xs) {
			x = xs[i]
		}
		if i < len(ys) {
			y = ys[i]
		}
		nx, ex := strconv.ParseUint(x, 10, 64)
		ny, ey := strconv.ParseUint(y, 10, 64)
		switch {
		case ex == nil && ey == nil:
			if nx != ny {
				if nx < ny {
					return -1
				}
				return 1
			}
		case ex == nil:
			return -1
		case ey == nil:
			return 1
		default:
			if c := strings.Compare(x, y); c != 0 {
				return c
			}
		}
	}
	return 0
}
