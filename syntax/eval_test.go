// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package syntax

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/grailbio/base/digest"
	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/types"
	"github.com/grailbio/reflow/values"
)

// eval parses, type checks, and then evaluates expression e
func eval(e string) (values.T, *types.T, *Session, error) {
	p := Parser{Body: bytes.NewReader([]byte(e)), Mode: ParseExpr}
	if err := p.Parse(); err != nil {
		return nil, nil, nil, err
	}
	tenv, venv := Stdlib()
	sess := NewSession()
	if err := p.Expr.Init(sess, tenv); err != nil {
		return nil, nil, nil, err
	}
	v, err := p.Expr.eval(sess, venv, "")
	return v, p.Expr.Type, sess, err
}

func TestEvalSimple(t *testing.T) {
	for _, c := range []struct {
		e string
		t *types.T
		v values.T
	}{
		{`1`, types.Int, values.NewInt(1)},
		{`-1`, types.Int, values.NewInt(-1)},
		{`"hello, world"`, types.String, "hello, world"},
		{
			`{a: 123, b: ([1,2], "ok")}`,
			types.Struct(
				&types.Field{Name: "a", T: types.Int},
				&types.Field{Name: "b", T: types.Tuple(
					&types.Field{T: types.List(types.Int)},
					&types.Field{T: types.String})}),
			values.Struct{
				"a": values.NewInt(123),
				"b": values.Tuple{values.List{values.NewInt(1), values.NewInt(2)}, "ok"},
			},
		},
		{`["foo": 123, "bar": 999]`, types.Map(types.String, types.Int),
			values.Map{"foo": values.NewInt(123), "bar": values.NewInt(999)}},
		{`if "foo" == "bar" { "no" } else { "yes" }`, types.String, "yes"},
		{`{x := {a: "blah", b:321};  x.a }`, types.String, "blah"},
		{`(func(x, y string) => x+y)("hello", "world")`, types.String, "helloworld"},
		{`{m := ["foo": 123, "bar": 333]; m["foo"]}`, types.Int, values.NewInt(123)},
		{
			`{val (x, y, [_, b], _) = (1, "ok", [true, false], "blah"); (x, y, b)}`,
			types.Tuple(
				&types.Field{T: types.Int},
				&types.Field{T: types.String},
				&types.Field{T: types.Bool}),
			values.Tuple{values.NewInt(1), "ok", false},
		},
	} {
		v, typ, _, err := eval(c.e)
		if err != nil {
			t.Errorf("eval %q: %v", c.e, err)
			continue
		}
		if got, want := typ, c.t; !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
			continue
		}
		if !values.Equal(v, c.v) {
			t.Errorf("got %v, want %v", values.Sprint(v, typ), values.Sprint(c.v, c.t))
		}
	}
}

func TestPat(t *testing.T) {
	_, _, _, err := eval(`{val [x, y] = [1]; 123}`)
	if got, want := err, errMatch; got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExec(t *testing.T) {
	v, typ, sess, err := eval(`
		exec(image := "ubuntu", mem := 32*GiB, cpu := 32) (out file) {"
			cat {{file("s3://blah")}} > {{out}}
		"}
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := typ, types.File; !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	f := v.(*reflow.Flow)
	if got, want := f.Op, reflow.OpCoerce; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := len(f.Deps), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	f = f.Deps[0]
	if got, want := f.Op, reflow.OpExec; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := len(f.Deps), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := f.Image, "ubuntu"; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := f.Resources, (reflow.Resources{CPU: 32, Disk: 0, Memory: 32 << 30}); got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := f.Cmd, "\n\t\t\tcat %s > %s\n\t\t"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := f.Argmap, []reflow.ExecArg{{Index: 0}, {Out: true, Index: 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := f.OutputIsDir, []bool{false}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	f = f.Deps[0]
	if got, want := f.Op, reflow.OpCoerce; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := len(f.Deps), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	f = f.Deps[0]
	if got, want := f.Op, reflow.OpK; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := len(f.Deps), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	f = f.Deps[0]
	if got, want := f.Op, reflow.OpCoerce; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := len(f.Deps), 1; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	f = f.Deps[0]
	if got, want := f.Op, reflow.OpIntern; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got, want := len(f.Deps), 0; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := f.URL.String(), "s3://blah"; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got, want := sess.Images(), []string{"ubuntu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEval(t *testing.T) {
	sess := NewSession()
	progs := []string{
		"testdata/test1.rf",
		"testdata/missingnewline.rf",
		"testdata/strings.rf",
		"testdata/path.rf",
		"testdata/typealias.rf",
		"testdata/typealias2.rf",
		"testdata/newmodule.rf",
		"testdata/delayed.rf",
	}
Prog:
	for _, prog := range progs {
		var err error
		m, err := sess.Open(prog)
		if err != nil {
			t.Errorf("%s: %v", prog, err)
			continue
		}
		var tests []string
		for _, f := range m.Type().Fields {
			if strings.HasPrefix(f.Name, "Test") {
				tests = append(tests, f.Name)
				if f.T.Kind != types.BoolKind {
					t.Errorf("%s.%s: tests must be boolean, not %s", prog, f.Name, f.T)
					continue Prog
				}
			}
		}
		if len(tests) == 0 {
			t.Errorf("%s: no tests", prog)
			continue
		}

		v, err := m.Make(sess, sess.Values)
		if err != nil {
			t.Errorf("make %s: %s", prog, err)
			continue
		}
	tests:
		for _, test := range tests {
			switch v := v.(values.Module)[test].(type) {
			case *reflow.Flow:
				// We have to evaluate the flow. We do so through a no-op executor.
				eval := reflow.NewEval(v, reflow.EvalConfig{
					Executor: nopexecutor{},
				})
				if err := eval.Do(context.Background()); err != nil {
					t.Errorf("%s.%s: %v", prog, test, err)
					continue tests
				}
				if !eval.Value().(bool) {
					t.Errorf("%s.%s failed", prog, test)
				}
			case bool:
				if !v {
					t.Errorf("%s.%s failed", prog, test)
				}
			}

		}
	}
}

func TestEvalErr(t *testing.T) {
	sess := NewSession()
	for _, c := range []struct {
		file string
		err  string
	}{
		{"testdata/err1.rf", "match error"},
		{"testdata/err2.rf", "panic: panic!"},
	} {
		m, err := sess.Open(c.file)
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		_, err = m.Make(sess, sess.Values)
		if err == nil {
			t.Errorf("%s: expected error", c.file)
			continue
		}
		if got, want := err.Error(), c.err; got != want {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestTypeErr(t *testing.T) {
	sess := NewSession()
	for _, c := range []struct {
		file   string
		errpat string
	}{
		{"testdata/typerr1.rf", `testdata/typerr1.rf:2:16: expected tuple of size 3, got 2 \(\(int, int, int\)\)$`},
		{"testdata/typerr2.rf", `testdata/typerr2.rf:5:3: expected list or map, got \{a, b, c int\}$`},
		{"testdata/typerr3.rf", `testdata/typerr3.rf:4:13: cannot use type file as type string in argument to F \(type func\(x, y, z string\) string\)$`},
		{"testdata/typerr4.rf", `typerr4.rf:5:16: failed to open module ./typerr4mod.rf: .*typerr4mod.rf:1:10: identifier "x" not defined$`},
		{"testdata/typerr5.rf", `typerr5.rf:1:16: failed to open module ./typerr5.reflow: param "invalid-parameter-name" is not a valid Reflow identifier`},
	} {
		_, terr := sess.Open(c.file)
		if terr == nil {
			t.Errorf("%s: expected error", c.file)
			continue
		}
		ok, err := regexp.MatchString(c.errpat, terr.Error())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("error %s did not match %s", terr, c.errpat)
		}
	}
}

type nopexecutor struct{}

func (nopexecutor) Put(ctx context.Context, id digest.Digest, exec reflow.ExecConfig) (reflow.Exec, error) {
	return nil, errors.New("put not implemented")
}

func (nopexecutor) Get(ctx context.Context, id digest.Digest) (reflow.Exec, error) {
	return nil, errors.New("get not implemented")
}

func (nopexecutor) Remove(ctx context.Context, id digest.Digest) error {
	return errors.New("remove not implemented")
}

func (nopexecutor) Execs(ctx context.Context) ([]reflow.Exec, error) {
	return nil, errors.New("execs not implemented")
}

func (nopexecutor) Resources() reflow.Resources {
	return reflow.MaxResources
}

func (nopexecutor) Repository() reflow.Repository {
	return nil
}
