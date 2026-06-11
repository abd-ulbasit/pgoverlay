package diffutil

import (
	"strings"
	"testing"
)

func TestUnifiedIdenticalInputsAreEmpty(t *testing.T) {
	for _, s := range []string{"", "a\n", "a\nb\nc\n"} {
		if got := Unified(s, s); got != "" {
			t.Errorf("Unified(%q, %q) = %q, want empty", s, s, got)
		}
	}
}

func TestUnifiedPureInsert(t *testing.T) {
	a := "one\ntwo\nthree\n"
	b := "one\ntwo\nnew line\nthree\n"
	got := Unified(a, b)
	want := strings.Join([]string{
		"@@ -1,3 +1,4 @@",
		" one",
		" two",
		"+new line",
		" three",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified insert:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedPureDelete(t *testing.T) {
	a := "one\ntwo\nthree\n"
	b := "one\nthree\n"
	got := Unified(a, b)
	want := strings.Join([]string{
		"@@ -1,3 +1,2 @@",
		" one",
		"-two",
		" three",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified delete:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedChange(t *testing.T) {
	a := "CREATE TABLE users (\n    id integer\n);\n"
	b := "CREATE TABLE users (\n    id bigint\n);\n"
	got := Unified(a, b)
	want := strings.Join([]string{
		"@@ -1,3 +1,3 @@",
		" CREATE TABLE users (",
		"-    id integer",
		"+    id bigint",
		" );",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified change:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedMultipleHunks(t *testing.T) {
	// two changes separated by far more than 2*context equal lines -> two hunks
	mid := make([]string, 20)
	for i := range mid {
		mid[i] = "same"
	}
	aLines := append(append([]string{"first-old"}, mid...), "last-old")
	bLines := append(append([]string{"first-new"}, mid...), "last-new")
	got := Unified(strings.Join(aLines, "\n")+"\n", strings.Join(bLines, "\n")+"\n")

	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Fatalf("hunks = %d, want 2:\n%s", n, got)
	}
	for _, want := range []string{"-first-old", "+first-new", "-last-old", "+last-new"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
	// context is limited: the 20 unchanged middle lines must not all appear
	if n := strings.Count(got, " same\n"); n > 6 {
		t.Errorf("context lines = %d, want <= 6 (3 per hunk side):\n%s", n, got)
	}
	// second hunk header points at the right region (line 19 onward)
	if !strings.Contains(got, "@@ -19,4 +19,4 @@") {
		t.Errorf("second hunk header wrong:\n%s", got)
	}
}

func TestUnifiedInsertIntoEmpty(t *testing.T) {
	got := Unified("", "a\nb\n")
	want := strings.Join([]string{
		"@@ -0,0 +1,2 @@",
		"+a",
		"+b",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified into empty:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDeleteAll(t *testing.T) {
	got := Unified("a\nb\n", "")
	want := strings.Join([]string{
		"@@ -1,2 +0,0 @@",
		"-a",
		"-b",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Unified delete all:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
