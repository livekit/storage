// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Each emulator test is a check of its own in CI, from the e2e job's matrix;
// one left out of it would run nowhere.
func TestWorkflowRunsEveryEmulatorTest(t *testing.T) {
	workflow, err := os.ReadFile("../.github/workflows/test.yaml")
	require.NoError(t, err)
	_, rest, ok := strings.Cut(string(workflow), "test: [")
	require.True(t, ok, "no e2e matrix")
	list, _, _ := strings.Cut(rest, "]")
	var matrix []string
	for _, name := range strings.Split(list, ",") {
		matrix = append(matrix, "Test"+strings.TrimSpace(name))
	}

	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	var tests []string
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && (strings.HasPrefix(fn.Name.Name, "TestFloci") || fn.Name.Name == "TestMinIO") {
				tests = append(tests, fn.Name.Name)
			}
		}
	}
	require.ElementsMatch(t, tests, matrix)
}
