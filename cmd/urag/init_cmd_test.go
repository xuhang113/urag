package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBootstrapFixture(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "bootstrap", "fixture.json")
	fx, err := loadBootstrapFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	if fx.CollectionID != "public" || len(fx.Documents) != 3 {
		t.Fatalf("%s %d", fx.CollectionID, len(fx.Documents))
	}
	if !strings.Contains(fx.Documents[0].Body, fx.ProbeQuery) {
		t.Fatal("probe is not in the first fixture")
	}
}

func TestInitCommandsRejectArgs(t *testing.T) {
	err := run([]string{"init-db", "extra"})
	if err == nil || !strings.Contains(err.Error(), "不接收参数") {
		t.Fatal(err)
	}
	err = run([]string{"init-es", "extra"})
	if err == nil || !strings.Contains(err.Error(), "不接收参数") {
		t.Fatal(err)
	}
}
