package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionWorkspaceDefaultsToCwd(t *testing.T) {
	got, err := sessionWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if got != cwd {
		t.Fatalf("workspace = %q, want the working directory %q", got, cwd)
	}
}

func TestSessionWorkspaceMakesTheFlagAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // Go 1.24+; otherwise save and restore os.Chdir by hand
	got, err := sessionWorkspace("sub/dir")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "sub", "dir"); got != want {
		t.Fatalf("workspace = %q, want %q", got, want)
	}
}

func TestTakeWorkspaceFlag(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantRest  []string
		wantWS    string
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "no flag at all",
			args:     []string{"prompt", "more"},
			wantRest: []string{"prompt", "more"},
			wantWS:   "",
		},
		{
			name:     "flag before the positional",
			args:     []string{"--workspace", "/x", "prompt"},
			wantRest: []string{"prompt"},
			wantWS:   "/x",
		},
		{
			name:     "flag after the positional",
			args:     []string{"prompt", "--workspace", "/x"},
			wantRest: []string{"prompt"},
			wantWS:   "/x",
		},
		{
			name:     "single-dash spelling",
			args:     []string{"-workspace", "/x", "prompt"},
			wantRest: []string{"prompt"},
			wantWS:   "/x",
		},
		{
			name:     "value beginning with a dash",
			args:     []string{"--workspace", "-weird-dir", "prompt"},
			wantRest: []string{"prompt"},
			wantWS:   "-weird-dir",
		},
		{
			name:     "dash-leading prompt survives untouched",
			args:     []string{"--workspace", "/x", "-not a flag"},
			wantRest: []string{"-not a flag"},
			wantWS:   "/x",
		},
		{
			name:     "flag given twice: last value wins",
			args:     []string{"--workspace", "/first", "prompt", "--workspace", "/second"},
			wantRest: []string{"prompt"},
			wantWS:   "/second",
		},
		{
			name:      "flag as the final argument with no value",
			args:      []string{"prompt", "--workspace"},
			wantErr:   true,
			errSubstr: "--workspace",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rest, ws, err := takeWorkspaceFlag(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("takeWorkspaceFlag(%v) = nil error, want one mentioning %q", tc.args, tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q does not mention %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("takeWorkspaceFlag(%v): unexpected error: %v", tc.args, err)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Fatalf("rest = %#v, want %#v", rest, tc.wantRest)
			}
			if ws != tc.wantWS {
				t.Fatalf("workspace = %q, want %q", ws, tc.wantWS)
			}
		})
	}
}
