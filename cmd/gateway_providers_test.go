package cmd

import (
	"reflect"
	"testing"
)

func TestResolveACPArgs_InjectsModelWhenMissing(t *testing.T) {
	got, injected := resolveACPArgs("gemini-3-flash-preview", []string{"--acp", "--include-directories", "/tmp"})

	want := []string{"--model", "gemini-3-flash-preview", "--acp", "--include-directories", "/tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if !injected {
		t.Error("injected = false, want true")
	}
}

func TestResolveACPArgs_PreservesExistingModelFlag(t *testing.T) {
	original := []string{"--acp", "--model", "gemini-2.5-pro"}
	got, injected := resolveACPArgs("gemini-3-flash-preview", original)

	if !reflect.DeepEqual(got, original) {
		t.Errorf("args = %v, want %v (unchanged)", got, original)
	}
	if injected {
		t.Error("injected = true, want false")
	}
}

func TestResolveACPArgs_PreservesExistingModelEqualFlag(t *testing.T) {
	original := []string{"--acp", "--model=gemini-2.5-pro"}
	got, injected := resolveACPArgs("gemini-3-flash-preview", original)

	if !reflect.DeepEqual(got, original) {
		t.Errorf("args = %v, want %v (unchanged)", got, original)
	}
	if injected {
		t.Error("injected = true, want false")
	}
}

func TestResolveACPArgs_DoesNotInjectWhenModelEmpty(t *testing.T) {
	original := []string{"--acp"}
	got, injected := resolveACPArgs("", original)

	if !reflect.DeepEqual(got, original) {
		t.Errorf("args = %v, want %v (unchanged)", got, original)
	}
	if injected {
		t.Error("injected = true, want false")
	}
}

func TestResolveACPArgs_NilArgsWithModel(t *testing.T) {
	got, injected := resolveACPArgs("gemini-3-flash-preview", nil)

	want := []string{"--model", "gemini-3-flash-preview"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
	if !injected {
		t.Error("injected = false, want true")
	}
}
