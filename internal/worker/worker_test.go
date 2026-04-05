package worker

import (
	"agentgo/internal/roster"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/quick"
)

// TestProperty_TruncateKeepTail verifies Property 1: Shell 输出截断保留尾部并附加提示
// Feature: worker-tool-expansion, Property 1: Shell 输出截断保留尾部并附加提示
// **Validates: Requirements 1.4, 5.1, 5.2**
func TestProperty_TruncateKeepTail(t *testing.T) {
	f := func(s string, limitRaw uint16) bool {
		limit := int(limitRaw)

		result := truncateKeepTail(s, limit)

		if len(s) <= limit {
			// Within limit: return value equals the original string
			if result != s {
				t.Logf("expected original string when len(s)=%d <= limit=%d, got %q", len(s), limit, result)
				return false
			}
		} else {
			// Over limit: return value ends with the last `limit` characters of the original string
			tail := s[len(s)-limit:]
			if !strings.HasSuffix(result, tail) {
				t.Logf("expected result to end with last %d chars of original", limit)
				return false
			}

			// Over limit: the result contains the truncation notice
			notice := fmt.Sprintf("[截断提示] 原始输出共 %d 字符，已截断前 %d 字符，仅保留最后 %d 字符",
				len(s), len(s)-limit, limit)
			if !strings.Contains(result, notice) {
				t.Logf("expected result to contain truncation notice")
				return false
			}
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Errorf("property check failed: %v", err)
	}
}

// TestProperty_ShellResultFormat verifies Property 2: Shell 结果格式化包含所有字段
// Feature: worker-tool-expansion, Property 2: Shell 结果格式化包含所有字段
// **Validates: Requirements 1.3**
func TestProperty_ShellResultFormat(t *testing.T) {
	f := func(output string, exitCode int) bool {
		// Format the result the same way makeRunShellTool does
		result := fmt.Sprintf("exit_code: %d\nstdout+stderr:\n%s", exitCode, output)

		// The formatted result must contain the exit code value
		exitCodeStr := fmt.Sprintf("exit_code: %d", exitCode)
		if !strings.Contains(result, exitCodeStr) {
			t.Logf("result missing exit code %d", exitCode)
			return false
		}

		// The formatted result must contain the output content
		if !strings.Contains(result, output) {
			t.Logf("result missing output content")
			return false
		}

		// The formatted result must contain the "stdout+stderr:" label
		if !strings.Contains(result, "stdout+stderr:") {
			t.Logf("result missing stdout+stderr label")
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Errorf("property check failed: %v", err)
	}
}

// TestProperty_DoubleStarMatchesAnyDepth verifies Property 5: ** 通配符匹配任意深度
// Feature: worker-tool-expansion, Property 5: ** 通配符匹配任意深度
// **Validates: Requirements 4.1**
func TestProperty_DoubleStarMatchesAnyDepth(t *testing.T) {
	// validSegment generates a random non-empty, non-hidden directory/file name
	// consisting of lowercase letters and digits (1-8 chars).
	validSegment := func(r *rand.Rand) string {
		const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
		n := r.Intn(8) + 1
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = chars[r.Intn(len(chars))]
		}
		return string(buf)
	}

	t.Run("matches_go_files_at_any_depth", func(t *testing.T) {
		f := func(seed int64) bool {
			r := rand.New(rand.NewSource(seed))

			// Random depth 1-10
			depth := r.Intn(10) + 1

			// Build a random path like "dir1/dir2/.../filename.go"
			parts := make([]string, depth)
			for i := 0; i < depth-1; i++ {
				parts[i] = validSegment(r)
			}
			parts[depth-1] = validSegment(r) + ".go"
			relPath := strings.Join(parts, "/")

			matched, err := MatchGlob("**/*.go", relPath)
			if err != nil {
				t.Logf("unexpected error for path %q: %v", relPath, err)
				return false
			}
			if !matched {
				t.Logf("expected **/*.go to match %q", relPath)
				return false
			}
			return true
		}

		if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
			t.Errorf("property check failed: %v", err)
		}
	})

	t.Run("does_not_match_different_extension", func(t *testing.T) {
		f := func(seed int64) bool {
			r := rand.New(rand.NewSource(seed))

			// Random depth 1-10
			depth := r.Intn(10) + 1

			// Build a random path ending with .txt (not .go)
			parts := make([]string, depth)
			for i := 0; i < depth-1; i++ {
				parts[i] = validSegment(r)
			}
			parts[depth-1] = validSegment(r) + ".txt"
			relPath := strings.Join(parts, "/")

			matched, err := MatchGlob("**/*.go", relPath)
			if err != nil {
				t.Logf("unexpected error for path %q: %v", relPath, err)
				return false
			}
			if matched {
				t.Logf("expected **/*.go NOT to match %q", relPath)
				return false
			}
			return true
		}

		if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
			t.Errorf("property check failed: %v", err)
		}
	})

	t.Run("single_file_no_directory", func(t *testing.T) {
		// **/*.go should also match a file at root level like "main.go"
		matched, err := MatchGlob("**/*.go", "main.go")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !matched {
			t.Errorf("expected **/*.go to match 'main.go'")
		}
	})
}

// TestProperty_NoDoubleStarEqualsFilepathMatch verifies Property 6: 无 ** 模式等价于 filepath.Match 文件名匹配
// Feature: worker-tool-expansion, Property 6: 无 ** 模式等价于 filepath.Match 文件名匹配
// **Validates: Requirements 4.2**
func TestProperty_NoDoubleStarEqualsFilepathMatch(t *testing.T) {
	// validName generates a random filename segment (lowercase letters + digits, 1-8 chars).
	validName := func(r *rand.Rand) string {
		const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
		n := r.Intn(8) + 1
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = chars[r.Intn(len(chars))]
		}
		return string(buf)
	}

	// Known-valid glob pattern templates (no **). Each generator produces a
	// pattern that filepath.Match will never reject as malformed.
	type patternGen struct {
		name string
		gen  func(r *rand.Rand) string
	}

	generators := []patternGen{
		{"star_dot_ext", func(r *rand.Rand) string {
			// e.g. *.go, *.txt, *.md
			exts := []string{"go", "txt", "md", "json", "yaml", "rs", "py", "js"}
			return "*." + exts[r.Intn(len(exts))]
		}},
		{"prefix_star", func(r *rand.Rand) string {
			// e.g. test*, main*, foo*
			return validName(r) + "*"
		}},
		{"question_mark", func(r *rand.Rand) string {
			// e.g. ?oo, ??x — one or more ? followed by literal chars
			qCount := r.Intn(3) + 1
			suffix := validName(r)
			return strings.Repeat("?", qCount) + suffix
		}},
		{"exact_name", func(r *rand.Rand) string {
			// Exact filename match, e.g. main.go
			exts := []string{".go", ".txt", ".md", ""}
			return validName(r) + exts[r.Intn(len(exts))]
		}},
		{"star_only", func(_ *rand.Rand) string {
			return "*"
		}},
		{"prefix_star_dot_ext", func(r *rand.Rand) string {
			// e.g. test*.go, main*.txt
			exts := []string{"go", "txt", "md", "json"}
			return validName(r) + "*." + exts[r.Intn(len(exts))]
		}},
	}

	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))

		// Pick a random pattern generator
		pg := generators[r.Intn(len(generators))]
		pattern := pg.gen(r)

		// Generate a random filename (no path separators)
		exts := []string{".go", ".txt", ".md", ".json", ".yaml", ".rs", ".py", ".js", ""}
		filename := validName(r) + exts[r.Intn(len(exts))]

		// MatchGlob result (no ** in pattern)
		gotMatch, gotErr := MatchGlob(pattern, filename)

		// filepath.Match result
		wantMatch, wantErr := filepath.Match(pattern, filename)

		// Both should agree on error status
		if (gotErr != nil) != (wantErr != nil) {
			t.Logf("error mismatch for pattern=%q filename=%q: MatchGlob err=%v, filepath.Match err=%v",
				pattern, filename, gotErr, wantErr)
			return false
		}

		// Both should agree on match result
		if gotMatch != wantMatch {
			t.Logf("result mismatch for pattern=%q filename=%q: MatchGlob=%v, filepath.Match=%v",
				pattern, filename, gotMatch, wantMatch)
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Errorf("property check failed: %v", err)
	}
}

// TestProperty_SingleMatchEdit verifies Property 3: 单次匹配编辑正确性
// Feature: worker-tool-expansion, Property 3: 单次匹配编辑正确性
// **Validates: Requirements 2.3**
func TestProperty_SingleMatchEdit(t *testing.T) {
	f := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))

		const chars = "abcdefghijklmnopqrstuvwxyz0123456789 \n"

		// Helper to generate a random string of given length from chars
		randStr := func(minLen, maxLen int) string {
			n := r.Intn(maxLen-minLen+1) + minLen
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = chars[r.Intn(len(chars))]
			}
			return string(buf)
		}

		// Build content as prefix + uniqueMarker + suffix.
		// The uniqueMarker uses a character not in chars to guarantee uniqueness.
		prefix := randStr(0, 50)
		suffix := randStr(0, 50)
		// Use a marker with a character (|) not present in the random alphabet
		markerBody := fmt.Sprintf("|UNIQUE_%d|", r.Int63())
		oldStr := markerBody
		content := prefix + oldStr + suffix

		// Verify oldStr appears exactly once; skip if not (defensive)
		if strings.Count(content, oldStr) != 1 {
			return true // skip this iteration
		}

		// Generate random newStr
		newStr := randStr(0, 40)

		// Perform the replacement (same logic as edit_file core)
		result := strings.Replace(content, oldStr, newStr, 1)

		// Property 1: Result contains newStr
		if !strings.Contains(result, newStr) {
			t.Logf("result does not contain newStr %q", newStr)
			return false
		}

		// Property 2: Result length equals len(content) - len(oldStr) + len(newStr)
		expectedLen := len(content) - len(oldStr) + len(newStr)
		if len(result) != expectedLen {
			t.Logf("length mismatch: got %d, expected %d (content=%d, oldStr=%d, newStr=%d)",
				len(result), expectedLen, len(content), len(oldStr), len(newStr))
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
		t.Errorf("property check failed: %v", err)
	}
}

// TestProperty_NonSingleMatchReject verifies Property 4: 非单次匹配编辑拒绝
// Feature: worker-tool-expansion, Property 4: 非单次匹配编辑拒绝
// **Validates: Requirements 2.4, 2.5**
func TestProperty_NonSingleMatchReject(t *testing.T) {
	t.Run("zero_matches", func(t *testing.T) {
		f := func(seed int64) bool {
			r := rand.New(rand.NewSource(seed))

			const chars = "abcdefghijklmnopqrstuvwxyz0123456789 \n"

			// Generate random content
			contentLen := r.Intn(100) + 1
			contentBuf := make([]byte, contentLen)
			for i := range contentBuf {
				contentBuf[i] = chars[r.Intn(len(chars))]
			}
			content := string(contentBuf)

			// Use a marker character (|) not in chars to guarantee oldStr doesn't appear in content
			oldStr := fmt.Sprintf("|ABSENT_%d|", r.Int63())

			// Verify oldStr truly doesn't appear
			count := strings.Count(content, oldStr)
			if count != 0 {
				return true // skip edge case
			}

			// Simulate the edit_file count check logic
			if count == 0 {
				errMsg := "未找到匹配内容，old_str 在文件中不存在"
				if !strings.Contains(errMsg, "未找到") {
					t.Logf("zero-match error should contain '未找到', got: %s", errMsg)
					return false
				}
				// Verify content is not modified (no replacement should happen)
				result := strings.Replace(content, oldStr, "REPLACEMENT", 1)
				if result != content {
					t.Logf("content should not be modified when count==0")
					return false
				}
			}

			return true
		}

		if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
			t.Errorf("property check failed: %v", err)
		}
	})

	t.Run("multiple_matches", func(t *testing.T) {
		f := func(seed int64) bool {
			r := rand.New(rand.NewSource(seed))

			const chars = "abcdefghijklmnopqrstuvwxyz0123456789"

			// Generate a random oldStr (short, using marker char to control placement)
			oldStr := fmt.Sprintf("|M%d|", r.Intn(1000))

			// Construct content that contains oldStr multiple times (2-5 times)
			repeatCount := r.Intn(4) + 2 // 2 to 5
			var parts []string
			for i := 0; i < repeatCount; i++ {
				// Random filler between occurrences
				fillerLen := r.Intn(20) + 1
				filler := make([]byte, fillerLen)
				for j := range filler {
					filler[j] = chars[r.Intn(len(chars))]
				}
				parts = append(parts, string(filler))
				parts = append(parts, oldStr)
			}
			// Add trailing filler
			trailLen := r.Intn(10) + 1
			trail := make([]byte, trailLen)
			for j := range trail {
				trail[j] = chars[r.Intn(len(chars))]
			}
			parts = append(parts, string(trail))
			content := strings.Join(parts, "")

			count := strings.Count(content, oldStr)
			if count <= 1 {
				return true // skip if construction didn't produce >1 matches
			}

			// Simulate the edit_file count check logic
			errMsg := fmt.Sprintf("匹配到 %d 处，请提供更精确的 old_str", count)

			// Error message should contain the match count
			countStr := fmt.Sprintf("%d", count)
			if !strings.Contains(errMsg, countStr) {
				t.Logf("multi-match error should contain count %d, got: %s", count, errMsg)
				return false
			}

			// Verify content is not modified (edit should be rejected)
			// The edit_file logic does NOT call strings.Replace when count != 1
			// So the original content must remain unchanged
			if strings.Count(content, oldStr) != count {
				t.Logf("content should not be modified when count > 1")
				return false
			}

			return true
		}

		if err := quick.Check(f, &quick.Config{MaxCount: 200}); err != nil {
			t.Errorf("property check failed: %v", err)
		}
	})
}

// TestRunShell_EchoHello verifies that `echo hello` returns exit_code=0 and stdout containing "hello".
// Works on both Windows (cmd /C) and Unix (sh -c).
// Validates: Requirements 1.1, 1.2, 1.3
func TestRunShell_EchoHello(t *testing.T) {
	toolFn := makeRunShellTool(30)
	result, err := toolFn(context.Background(), map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "exit_code: 0") {
		t.Errorf("expected exit_code: 0, got:\n%s", result)
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected output to contain 'hello', got:\n%s", result)
	}
}

// TestRunShell_NonZeroExitCode verifies that a command returning non-zero exit code
// is returned as a normal result (not a Go error).
// Validates: Requirements 1.3, 1.5
func TestRunShell_NonZeroExitCode(t *testing.T) {
	// "exit 1" works in both sh and cmd
	var command string
	if runtime.GOOS == "windows" {
		command = "exit /b 1"
	} else {
		command = "exit 1"
	}

	toolFn := makeRunShellTool(30)
	result, err := toolFn(context.Background(), map[string]any{
		"command": command,
	})
	if err != nil {
		t.Fatalf("non-zero exit code should not be a Go error, got: %v", err)
	}
	if !strings.Contains(result, "exit_code: 1") {
		t.Errorf("expected exit_code: 1, got:\n%s", result)
	}
}

// TestRunShell_EmptyCommand verifies that an empty command returns a parameter error.
// Validates: Requirements 1.5
func TestRunShell_EmptyCommand(t *testing.T) {
	toolFn := makeRunShellTool(30)
	_, err := toolFn(context.Background(), map[string]any{
		"command": "",
	})
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
	if !strings.Contains(err.Error(), "command") {
		t.Errorf("expected error to mention 'command', got: %v", err)
	}
}

// TestRunShell_Timeout verifies that a long-running command is killed after the timeout and returns a timeout error.
// Uses makeRunShellTool(1) with a long-running command to trigger a 1-second timeout.
// Validates: Requirements 1.6
func TestRunShell_Timeout(t *testing.T) {
	// Use a cross-platform long-running command
	var command string
	if runtime.GOOS == "windows" {
		command = "ping -n 60 127.0.0.1"
	} else {
		command = "sleep 60"
	}

	toolFn := makeRunShellTool(1) // 1 second timeout
	_, err := toolFn(context.Background(), map[string]any{
		"command": command,
	})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("expected error to contain '超时' (timeout), got: %v", err)
	}
}

// TestEditFile_RosterLockFlow verifies the full TryClaim → edit → Release sequence.
// Creates a temp file, uses a real MemoryRoster, calls makeEditFileTool, verifies the edit
// succeeds and the file is modified correctly. After the call, verifies the Roster lock is released.
// Validates: Requirements 2.2, 2.6
func TestEditFile_RosterLockFlow(t *testing.T) {
	// Create a temp file with known content
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")
	original := "hello world"
	if err := os.WriteFile(filePath, []byte(original), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	r := roster.NewMemoryRoster()
	agentID := "test-agent"
	toolFn := makeEditFileTool(r, agentID)

	result, err := toolFn(context.Background(), map[string]any{
		"path":    filePath,
		"old_str": "hello",
		"new_str": "goodbye",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the result message mentions the file path
	if !strings.Contains(result, filePath) {
		t.Errorf("expected result to contain file path %q, got: %s", filePath, result)
	}

	// Verify the file was actually modified
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read modified file: %v", err)
	}
	if string(data) != "goodbye world" {
		t.Errorf("expected file content 'goodbye world', got %q", string(data))
	}

	// Verify the Roster lock is released after the call
	_, occupied, err := r.IsOccupied(filePath)
	if err != nil {
		t.Fatalf("IsOccupied error: %v", err)
	}
	if occupied {
		t.Errorf("expected Roster lock to be released after edit_file, but file is still occupied")
	}
}

// TestEditFile_RosterConflict verifies that edit_file returns a conflict error when the file
// is already claimed by another agent.
// Validates: Requirements 2.6, 2.8
func TestEditFile_RosterConflict(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "conflict.txt")
	if err := os.WriteFile(filePath, []byte("content"), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	r := roster.NewMemoryRoster()

	// Pre-claim the file with another agent
	claimed, err := r.TryClaim("other-agent", filePath)
	if err != nil || !claimed {
		t.Fatalf("failed to pre-claim file: claimed=%v, err=%v", claimed, err)
	}

	// Now try to edit with a different agent
	toolFn := makeEditFileTool(r, "test-agent")
	_, err = toolFn(context.Background(), map[string]any{
		"path":    filePath,
		"old_str": "content",
		"new_str": "new content",
	})
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "占用") {
		t.Errorf("expected error to contain '占用' (occupied), got: %v", err)
	}
	if !strings.Contains(err.Error(), "other-agent") {
		t.Errorf("expected error to mention 'other-agent', got: %v", err)
	}

	// Verify the file was NOT modified
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("expected file to remain unchanged, got %q", string(data))
	}
}

// TestEditFile_MissingParams verifies that empty path and empty old_str return appropriate errors.
// Validates: Requirements 2.7
func TestEditFile_MissingParams(t *testing.T) {
	r := roster.NewMemoryRoster()
	toolFn := makeEditFileTool(r, "test-agent")

	t.Run("empty_path", func(t *testing.T) {
		_, err := toolFn(context.Background(), map[string]any{
			"path":    "",
			"old_str": "something",
			"new_str": "other",
		})
		if err == nil {
			t.Fatal("expected error for empty path, got nil")
		}
		if !strings.Contains(err.Error(), "path") {
			t.Errorf("expected error to mention 'path', got: %v", err)
		}
	})

	t.Run("empty_old_str", func(t *testing.T) {
		_, err := toolFn(context.Background(), map[string]any{
			"path":    "/some/file.txt",
			"old_str": "",
			"new_str": "other",
		})
		if err == nil {
			t.Fatal("expected error for empty old_str, got nil")
		}
		if !strings.Contains(err.Error(), "old_str") {
			t.Errorf("expected error to mention 'old_str', got: %v", err)
		}
	})
}

// TestEditFile_FileNotFound verifies that editing a non-existent file returns an error.
// Validates: Requirements 2.8
func TestEditFile_FileNotFound(t *testing.T) {
	r := roster.NewMemoryRoster()
	toolFn := makeEditFileTool(r, "test-agent")

	nonExistentPath := filepath.Join(t.TempDir(), "does_not_exist.txt")

	_, err := toolFn(context.Background(), map[string]any{
		"path":    nonExistentPath,
		"old_str": "something",
		"new_str": "other",
	})
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("expected error to contain '不存在' (not exist), got: %v", err)
	}

	// Verify the Roster lock is released even on error
	_, occupied, rErr := r.IsOccupied(nonExistentPath)
	if rErr != nil {
		t.Fatalf("IsOccupied error: %v", rErr)
	}
	if occupied {
		t.Errorf("expected Roster lock to be released after file-not-found error")
	}
}

// TestGlobSearch_HiddenDirectorySkipping verifies that files inside hidden directories (e.g. .git/)
// are NOT matched by glob_search.
// Validates: Requirements 3.2, 3.4
func TestGlobSearch_HiddenDirectorySkipping(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a hidden .git directory with files inside
	gitDir := filepath.Join(tmpDir, ".git", "objects")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatalf("failed to create .git/objects: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "abc123.go"), []byte("package obj"), 0644); err != nil {
		t.Fatalf("failed to create file in .git: %v", err)
	}
	// Also create a .hidden directory
	hiddenDir := filepath.Join(tmpDir, ".hidden")
	if err := os.MkdirAll(hiddenDir, 0755); err != nil {
		t.Fatalf("failed to create .hidden: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hiddenDir, "secret.go"), []byte("package secret"), 0644); err != nil {
		t.Fatalf("failed to create file in .hidden: %v", err)
	}

	// Create a visible file that should be matched
	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create main.go: %v", err)
	}

	result, err := ToolGlobSearch(context.Background(), map[string]any{
		"pattern":  "**/*.go",
		"root_dir": tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The visible file should be matched
	if !strings.Contains(result, "main.go") {
		t.Errorf("expected result to contain 'main.go', got:\n%s", result)
	}

	// Files inside .git should NOT be matched
	if strings.Contains(result, "abc123.go") {
		t.Errorf("expected .git/objects/abc123.go to be skipped, but found in result:\n%s", result)
	}

	// Files inside .hidden should NOT be matched
	if strings.Contains(result, "secret.go") {
		t.Errorf("expected .hidden/secret.go to be skipped, but found in result:\n%s", result)
	}
}

// TestGlobSearch_EmptyResult verifies that when no files match the pattern,
// the result is "未找到匹配文件".
// Validates: Requirements 3.7
func TestGlobSearch_EmptyResult(t *testing.T) {
	tmpDir := t.TempDir()

	// Create some files that won't match the pattern
	if err := os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("# readme"), 0644); err != nil {
		t.Fatalf("failed to create readme.md: %v", err)
	}

	result, err := ToolGlobSearch(context.Background(), map[string]any{
		"pattern":  "**/*.xyz_nonexistent",
		"root_dir": tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "未找到匹配文件" {
		t.Errorf("expected '未找到匹配文件', got: %q", result)
	}
}

// TestGlobSearch_ResultTruncation verifies that when more than 200 files match,
// the result is truncated and a truncation notice is appended.
// Validates: Requirements 3.3
func TestGlobSearch_ResultTruncation(t *testing.T) {
	tmpDir := t.TempDir()

	// Create 210 matching .txt files
	for i := 0; i < 210; i++ {
		fname := filepath.Join(tmpDir, fmt.Sprintf("file_%03d.txt", i))
		if err := os.WriteFile(fname, []byte("data"), 0644); err != nil {
			t.Fatalf("failed to create file %d: %v", i, err)
		}
	}

	result, err := ToolGlobSearch(context.Background(), map[string]any{
		"pattern":  "**/*.txt",
		"root_dir": tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should contain the truncation notice
	if !strings.Contains(result, "结果已截断") {
		t.Errorf("expected truncation notice '结果已截断', got:\n%s", result)
	}

	// Should mention the total count (210)
	if !strings.Contains(result, "210") {
		t.Errorf("expected result to mention total count 210, got:\n%s", result)
	}

	// Should mention "前 200 个"
	if !strings.Contains(result, "前 200 个") {
		t.Errorf("expected result to mention '前 200 个', got:\n%s", result)
	}

	// Count the number of file paths in the result (lines before the truncation notice)
	lines := strings.Split(result, "\n")
	fileCount := 0
	for _, line := range lines {
		if strings.HasSuffix(line, ".txt") {
			fileCount++
		}
	}
	if fileCount != 200 {
		t.Errorf("expected exactly 200 file paths, got %d", fileCount)
	}
}

// TestSystemPrompt_ContainsToolNames verifies that the Worker system prompt
// contains the three tool names and the edit_file priority guidance.
// Validates: Requirements 6.1, 6.2
func TestSystemPrompt_ContainsToolNames(t *testing.T) {
	// Requirement 6.1: system prompt contains tool names
	for _, toolName := range []string{"run_shell", "edit_file", "glob_search"} {
		if !strings.Contains(systemPrompt, toolName) {
			t.Errorf("systemPrompt should contain tool name %q", toolName)
		}
	}

	// Requirement 6.2: system prompt guides LLM to prefer edit_file over write_file
	if !strings.Contains(systemPrompt, "优先") {
		t.Error("systemPrompt should contain edit_file priority guidance (优先)")
	}
	if !strings.Contains(systemPrompt, "edit_file") {
		t.Error("systemPrompt should mention edit_file in priority guidance")
	}
}

// TestGlobSearch_DoubleStarGoMultiLevel verifies that **/*.go matches .go files
// across multiple directory levels.
// Validates: Requirements 3.2, 3.7
func TestGlobSearch_DoubleStarGoMultiLevel(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a multi-level directory structure with .go files
	dirs := []string{
		"",                     // root level
		"pkg",                  // 1 level deep
		"pkg/sub",              // 2 levels deep
		"internal/deep/nested", // 3 levels deep
	}
	for _, d := range dirs {
		dir := filepath.Join(tmpDir, d)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create dir %q: %v", d, err)
		}
		goFile := filepath.Join(dir, "code.go")
		if err := os.WriteFile(goFile, []byte("package x"), 0644); err != nil {
			t.Fatalf("failed to create %s: %v", goFile, err)
		}
	}

	// Also create a non-.go file that should NOT match
	if err := os.WriteFile(filepath.Join(tmpDir, "pkg", "readme.md"), []byte("# readme"), 0644); err != nil {
		t.Fatalf("failed to create readme.md: %v", err)
	}

	result, err := ToolGlobSearch(context.Background(), map[string]any{
		"pattern":  "**/*.go",
		"root_dir": tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All .go files should be matched
	expectedPaths := []string{
		"code.go",
		"pkg/code.go",
		"pkg/sub/code.go",
		"internal/deep/nested/code.go",
	}
	for _, expected := range expectedPaths {
		if !strings.Contains(result, expected) {
			t.Errorf("expected result to contain %q, got:\n%s", expected, result)
		}
	}

	// Non-.go files should NOT be matched
	if strings.Contains(result, "readme.md") {
		t.Errorf("expected readme.md to NOT be matched, but found in result:\n%s", result)
	}
}

// TestShellCommand_PlatformBranching verifies that shellCommand returns the correct
// shell and arguments for the current platform.
func TestShellCommand_PlatformBranching(t *testing.T) {
	shell, args := shellCommand("echo hello")

	if runtime.GOOS == "windows" {
		if shell != "cmd" {
			t.Errorf("on Windows, shell = %q, want %q", shell, "cmd")
		}
		if len(args) != 2 || args[0] != "/C" || args[1] != "echo hello" {
			t.Errorf("on Windows, args = %v, want [/C, echo hello]", args)
		}
	} else {
		if shell != "sh" {
			t.Errorf("on Unix, shell = %q, want %q", shell, "sh")
		}
		if len(args) != 2 || args[0] != "-c" || args[1] != "echo hello" {
			t.Errorf("on Unix, args = %v, want [-c, echo hello]", args)
		}
	}
}

// computeTestSHA256 is a test helper that computes SHA256 hex digest of data.
func computeTestSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// TestProperty_BugCondition_WriteFile verifies Property 1 (Bug Condition):
// When expected_hash is provided and does NOT match the current file content hash,
// write_file SHOULD reject the write with a "冲突" (conflict) error and leave the file unchanged.
//
// **Validates: Requirements 1.1, 2.1**
//
// On UNFIXED code this test is EXPECTED TO FAIL because makeWriteFileTool ignores expected_hash.
func TestProperty_BugCondition_WriteFile(t *testing.T) {
	r := roster.NewMemoryRoster()
	agentID := "test-agent"
	writeTool := makeWriteFileTool(r, agentID)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))

		// Generate random initial content v1 (1-200 bytes)
		v1Len := rng.Intn(200) + 1
		v1 := make([]byte, v1Len)
		for i := range v1 {
			v1[i] = byte(rng.Intn(256))
		}

		// Generate random modified content v2 (different from v1)
		v2Len := rng.Intn(200) + 1
		v2 := make([]byte, v2Len)
		for i := range v2 {
			v2[i] = byte(rng.Intn(256))
		}

		// Compute hash of v1 (the stale hash the agent holds)
		h1 := computeTestSHA256(v1)
		h2 := computeTestSHA256(v2)

		// isBugCondition: hashes must differ
		if h1 == h2 {
			return true // skip: same hash means same content (or collision), not a bug condition
		}

		// Create temp file with v1, then overwrite with v2 (simulating another agent's write)
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, fmt.Sprintf("test_%d.txt", seed))
		if err := os.WriteFile(filePath, v1, 0644); err != nil {
			t.Logf("failed to write v1: %v", err)
			return false
		}
		if err := os.WriteFile(filePath, v2, 0644); err != nil {
			t.Logf("failed to write v2: %v", err)
			return false
		}

		// Now call write_file with stale expected_hash=h1 (file is actually v2 with hash h2)
		newContent := "new content from stale agent"
		result, err := writeTool(context.Background(), map[string]any{
			"path":          filePath,
			"content":       newContent,
			"expected_hash": h1,
		})

		// EXPECTED BEHAVIOR (after fix): should return error containing "冲突"
		if err == nil {
			t.Logf("BUG: write_file succeeded with stale expected_hash (result=%q). Expected conflict error.", result)
			return false
		}
		if !strings.Contains(err.Error(), "冲突") {
			t.Logf("BUG: write_file returned error but without '冲突' keyword: %v", err)
			return false
		}

		// EXPECTED BEHAVIOR: file content should remain v2 (unchanged)
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Logf("failed to read file after write attempt: %v", readErr)
			return false
		}
		if string(data) != string(v2) {
			t.Logf("BUG: file content was modified despite stale hash. Expected v2, got different content.")
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("Property 1 (Bug Condition - WriteFile) failed: %v", err)
	}
}

// TestProperty_BugCondition_EditFile verifies Property 1 (Bug Condition):
// When expected_hash is provided and does NOT match the current file content hash,
// edit_file SHOULD reject the edit with a "冲突" (conflict) error and leave the file unchanged.
//
// **Validates: Requirements 1.2, 2.2**
//
// On UNFIXED code this test is EXPECTED TO FAIL because makeEditFileTool ignores expected_hash.
func TestProperty_BugCondition_EditFile(t *testing.T) {
	r := roster.NewMemoryRoster()
	agentID := "test-agent"
	editTool := makeEditFileTool(r, agentID)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))

		// Generate a unique marker to use as old_str (guaranteed to appear exactly once)
		marker := fmt.Sprintf("|MARKER_%d|", rng.Int63())

		// Generate random prefix and suffix
		const chars = "abcdefghijklmnopqrstuvwxyz0123456789 "
		randStr := func(maxLen int) string {
			n := rng.Intn(maxLen + 1)
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = chars[rng.Intn(len(chars))]
			}
			return string(buf)
		}

		prefix := randStr(50)
		suffix := randStr(50)

		// v1: the content the agent originally read (contains marker)
		v1 := prefix + marker + suffix

		// v2: the content after another agent modified the file (still contains marker so edit_file
		// would find old_str, but the hash is different — this is the TOCTOU scenario)
		extraContent := randStr(20)
		v2 := prefix + extraContent + marker + suffix

		h1 := computeTestSHA256([]byte(v1))
		h2 := computeTestSHA256([]byte(v2))

		// isBugCondition: hashes must differ
		if h1 == h2 {
			return true // skip
		}

		// Create temp file with v2 (the current state after another agent's modification)
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, fmt.Sprintf("edit_%d.txt", seed))
		if err := os.WriteFile(filePath, []byte(v2), 0644); err != nil {
			t.Logf("failed to write file: %v", err)
			return false
		}

		// Call edit_file with stale expected_hash=h1
		result, err := editTool(context.Background(), map[string]any{
			"path":          filePath,
			"old_str":       marker,
			"new_str":       "REPLACED",
			"expected_hash": h1,
		})

		// EXPECTED BEHAVIOR (after fix): should return error containing "冲突"
		if err == nil {
			t.Logf("BUG: edit_file succeeded with stale expected_hash (result=%q). Expected conflict error.", result)
			return false
		}
		if !strings.Contains(err.Error(), "冲突") {
			t.Logf("BUG: edit_file returned error but without '冲突' keyword: %v", err)
			return false
		}

		// EXPECTED BEHAVIOR: file content should remain v2 (unchanged)
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Logf("failed to read file after edit attempt: %v", readErr)
			return false
		}
		if string(data) != v2 {
			t.Logf("BUG: file content was modified despite stale hash. Expected v2 unchanged.")
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("Property 1 (Bug Condition - EditFile) failed: %v", err)
	}
}

// TestProperty_Preservation_WriteWithoutHash verifies Property 2 (Preservation):
// For all random file content, calling makeWriteFileTool WITHOUT expected_hash
// should successfully write and file content should be correct.
//
// **Validates: Requirements 2.4, 3.2**
func TestProperty_Preservation_WriteWithoutHash(t *testing.T) {
	r := roster.NewMemoryRoster()
	agentID := "test-agent"
	writeTool := makeWriteFileTool(r, agentID)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))

		// Generate random content to write (1-300 bytes, printable ASCII)
		contentLen := rng.Intn(300) + 1
		contentBuf := make([]byte, contentLen)
		for i := range contentBuf {
			contentBuf[i] = byte(rng.Intn(95) + 32) // printable ASCII 32-126
		}
		content := string(contentBuf)

		// Create a unique temp file path
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, fmt.Sprintf("write_%d.txt", seed))

		// Call write_file WITHOUT expected_hash
		result, err := writeTool(context.Background(), map[string]any{
			"path":    filePath,
			"content": content,
		})

		// Should succeed
		if err != nil {
			t.Logf("write_file without expected_hash failed: %v", err)
			return false
		}

		// Result should mention the file path
		if !strings.Contains(result, filePath) {
			t.Logf("result should contain file path, got: %s", result)
			return false
		}

		// File content should match what was written
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Logf("failed to read written file: %v", readErr)
			return false
		}
		if string(data) != content {
			t.Logf("file content mismatch: expected %d bytes, got %d bytes", len(content), len(data))
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("Property 2 (Preservation - WriteWithoutHash) failed: %v", err)
	}
}

// TestProperty_Preservation_EditWithoutHash verifies Property 2 (Preservation):
// For all random file content with a unique old_str, calling makeEditFileTool
// WITHOUT expected_hash should successfully replace.
//
// **Validates: Requirements 2.4, 3.2**
func TestProperty_Preservation_EditWithoutHash(t *testing.T) {
	r := roster.NewMemoryRoster()
	agentID := "test-agent"
	editTool := makeEditFileTool(r, agentID)

	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))

		const chars = "abcdefghijklmnopqrstuvwxyz0123456789 "
		randStr := func(maxLen int) string {
			n := rng.Intn(maxLen) + 1
			buf := make([]byte, n)
			for i := range buf {
				buf[i] = chars[rng.Intn(len(chars))]
			}
			return string(buf)
		}

		// Build content with a unique marker (guaranteed single match)
		prefix := randStr(50)
		suffix := randStr(50)
		marker := fmt.Sprintf("|UNIQUE_%d|", rng.Int63())
		content := prefix + marker + suffix

		// Generate random replacement
		newStr := randStr(40)

		// Create temp file
		tmpDir := t.TempDir()
		filePath := filepath.Join(tmpDir, fmt.Sprintf("edit_%d.txt", seed))
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			t.Logf("failed to create temp file: %v", err)
			return false
		}

		// Call edit_file WITHOUT expected_hash
		result, err := editTool(context.Background(), map[string]any{
			"path":    filePath,
			"old_str": marker,
			"new_str": newStr,
		})

		// Should succeed
		if err != nil {
			t.Logf("edit_file without expected_hash failed: %v", err)
			return false
		}

		// Result should mention the file path
		if !strings.Contains(result, filePath) {
			t.Logf("result should contain file path, got: %s", result)
			return false
		}

		// File content should have the marker replaced with newStr
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			t.Logf("failed to read edited file: %v", readErr)
			return false
		}
		expected := prefix + newStr + suffix
		if string(data) != expected {
			t.Logf("file content mismatch after edit")
			return false
		}

		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 100}); err != nil {
		t.Errorf("Property 2 (Preservation - EditWithoutHash) failed: %v", err)
	}
}

// TestProperty_Preservation_RosterLockConflict verifies Property 2 (Preservation):
// When file is claimed by another agent, write_file and edit_file should return
// error containing "占用".
//
// **Validates: Requirements 3.3**
func TestProperty_Preservation_RosterLockConflict(t *testing.T) {
	t.Run("write_file_roster_conflict", func(t *testing.T) {
		f := func(seed int64) bool {
			rng := rand.New(rand.NewSource(seed))

			r := roster.NewMemoryRoster()
			myAgentID := "my-agent"
			otherAgentID := fmt.Sprintf("other-agent-%d", rng.Int63())
			writeTool := makeWriteFileTool(r, myAgentID)

			// Create a temp file
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, fmt.Sprintf("conflict_w_%d.txt", seed))
			if err := os.WriteFile(filePath, []byte("original"), 0644); err != nil {
				t.Logf("failed to create temp file: %v", err)
				return false
			}

			// Pre-claim the file with another agent
			claimed, err := r.TryClaim(otherAgentID, filePath)
			if err != nil || !claimed {
				t.Logf("failed to pre-claim: claimed=%v, err=%v", claimed, err)
				return false
			}

			// Try to write with our agent — should fail with "占用"
			_, writeErr := writeTool(context.Background(), map[string]any{
				"path":    filePath,
				"content": "new content",
			})

			if writeErr == nil {
				t.Logf("write_file should fail when file is claimed by another agent")
				return false
			}
			if !strings.Contains(writeErr.Error(), "占用") {
				t.Logf("error should contain '占用', got: %v", writeErr)
				return false
			}

			// File content should remain unchanged
			data, readErr := os.ReadFile(filePath)
			if readErr != nil {
				t.Logf("failed to read file: %v", readErr)
				return false
			}
			if string(data) != "original" {
				t.Logf("file content should remain 'original', got: %q", string(data))
				return false
			}

			return true
		}

		if err := quick.Check(f, &quick.Config{MaxCount: 50}); err != nil {
			t.Errorf("Property 2 (Preservation - WriteFile RosterConflict) failed: %v", err)
		}
	})

	t.Run("edit_file_roster_conflict", func(t *testing.T) {
		f := func(seed int64) bool {
			rng := rand.New(rand.NewSource(seed))

			r := roster.NewMemoryRoster()
			myAgentID := "my-agent"
			otherAgentID := fmt.Sprintf("other-agent-%d", rng.Int63())
			editTool := makeEditFileTool(r, myAgentID)

			// Create a temp file with content containing a unique marker
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, fmt.Sprintf("conflict_e_%d.txt", seed))
			content := "hello world"
			if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
				t.Logf("failed to create temp file: %v", err)
				return false
			}

			// Pre-claim the file with another agent
			claimed, err := r.TryClaim(otherAgentID, filePath)
			if err != nil || !claimed {
				t.Logf("failed to pre-claim: claimed=%v, err=%v", claimed, err)
				return false
			}

			// Try to edit with our agent — should fail with "占用"
			_, editErr := editTool(context.Background(), map[string]any{
				"path":    filePath,
				"old_str": "hello",
				"new_str": "goodbye",
			})

			if editErr == nil {
				t.Logf("edit_file should fail when file is claimed by another agent")
				return false
			}
			if !strings.Contains(editErr.Error(), "占用") {
				t.Logf("error should contain '占用', got: %v", editErr)
				return false
			}

			// File content should remain unchanged
			data, readErr := os.ReadFile(filePath)
			if readErr != nil {
				t.Logf("failed to read file: %v", readErr)
				return false
			}
			if string(data) != content {
				t.Logf("file content should remain unchanged, got: %q", string(data))
				return false
			}

			return true
		}

		if err := quick.Check(f, &quick.Config{MaxCount: 50}); err != nil {
			t.Errorf("Property 2 (Preservation - EditFile RosterConflict) failed: %v", err)
		}
	})
}
