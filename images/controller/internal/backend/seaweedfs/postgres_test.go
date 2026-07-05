/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package seaweedfs

import (
	"strings"
	"testing"
)

func TestTomlBasicEscape(t *testing.T) {
	cases := map[string]string{
		"simple":       "simple",
		`a"b`:          `a\"b`,
		`a\b`:          `a\\b`,
		"a\nb":         `a\nb`,
		"a\tb":         `a\tb`,
		"a\rb":         `a\rb`,
		`p@ss"; evil=`: `p@ss\"; evil=`,
	}
	for in, want := range cases {
		if got := tomlBasicEscape(in); got != want {
			t.Errorf("tomlBasicEscape(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestRenderFilerTomlEscapesPassword(t *testing.T) {
	// A password with a double quote must not break out of the TOML string.
	toml := renderFilerToml("h", pgPort, "u", `pa"ss`, "db")
	if !strings.Contains(toml, `password = "pa\"ss"`) {
		t.Errorf("password not escaped in filer.toml:\n%s", toml)
	}
	// The createTable %s placeholder must survive as a literal for SeaweedFS.
	if !strings.Contains(toml, `"%s"`) {
		t.Errorf("createTable %%s placeholder missing in:\n%s", toml)
	}
}
