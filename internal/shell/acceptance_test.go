package shell

import "testing"

// hardenedAcceptanceFilter 构造验收语境的派生过滤器：
// 默认黑/灰名单 + 验收加固灰名单（与 tools.ShellGroup.Register 的接线一致）。
func hardenedAcceptanceFilter() *CommandFilter {
	return NewCommandFilter(DefaultBlacklist, DefaultGreylist).
		DeriveWithExtraGreylist(AcceptanceHardeningGreylist)
}

// 验收加固：写倾向命令一律升级为灰名单 ask（Interaction 审批通道）。
func TestAcceptanceHardening_WriteishCommandsAsk(t *testing.T) {
	filter := hardenedAcceptanceFilter()
	cases := []string{
		// 版本控制写
		"git add .",
		"git commit -m \"wip\"",
		"git checkout main",
		"git reset --soft HEAD~1",
		"git clean -fd",
		"git rm cached.txt",
		"git push origin main",
		"git pull",
		"git switch feature",
		"git restore .",
		// 包管理与依赖写
		"npm install",
		"npm install -g eslint",
		"npm ci",
		"pip install requests",
		"python -m pip install requests",
		"go get example.com/mod",
		"go mod tidy",
		"apt install curl",
		"apt-get remove curl",
		"brew install jq",
		"yarn add lodash",
		"pnpm install",
		"cargo add serde",
		"dotnet add package Newtonsoft.Json",
		// 删除 / 移动 / 复制（双方言）
		"rm foo.txt",
		"rm -f foo.txt",
		"rmdir old",
		"Remove-Item foo.txt",
		"del foo.txt",
		"mv a b",
		"Move-Item a b",
		"Rename-Item a b",
		"cp a b",
		"Copy-Item a b",
		"robocopy src dst",
		// 直接文件内容写
		"Set-Content a.txt x",
		"Add-Content a.txt x",
		"sed -i 's/a/b/' f.go",
		"sed -i.bak 's/a/b/' f.go",
		"sed -Ei 's/a/b/' f.go",
		"sed --in-place 's/a/b/' f.go",
	}
	for _, cmd := range cases {
		if action, pattern := filter.Check(cmd); action != "ask" {
			t.Errorf("Check(%q) = (%s, %q)，期望 ask", cmd, action, pattern)
		}
	}
}

// 验收加固不得误伤验收正当命令：测试 / 构建 / 静态检查 / 只读 git /
// 文本搜索 / 一般读操作保持 allow。
func TestAcceptanceHardening_LegitVerificationCommandsAllow(t *testing.T) {
	filter := hardenedAcceptanceFilter()
	cases := []string{
		"go test ./...",
		"go build ./...",
		"go vet ./...",
		"go version",
		"gofmt -l .",
		"git status",
		"git diff HEAD",
		"git log --oneline -5",
		"git show HEAD",
		"git branch",
		"git remote -v",
		"grep -ri \"pattern\" ./internal",
		"grep -rn \"func main\" .",
		"Select-String -Path a.go -Pattern \"func\"",
		"ls -la",
		"cat go.mod",
		"Get-Content go.mod",
		"find . -name \"*.go\"",
		"echo done",
		"dotnet test",
		"mvn test",
	}
	for _, cmd := range cases {
		if action, pattern := filter.Check(cmd); action != "allow" {
			t.Errorf("Check(%q) = (%s, %q)，期望 allow（验收正当命令不得误伤）", cmd, action, pattern)
		}
	}
}

// 与默认灰名单重叠的命令仍报告默认模式（匹配顺序：先默认、后加固），
// allow_session 捕获的语义与单过滤器时期一致。
func TestAcceptanceHardening_OverlapPrefersDefaultPattern(t *testing.T) {
	filter := hardenedAcceptanceFilter()
	action, pattern := filter.Check("git push origin main")
	if action != "ask" || pattern != `git\s+push` {
		t.Errorf("Check(git push) = (%s, %q)，期望 ask + 默认模式 git\\s+push", action, pattern)
	}
	action, pattern = filter.Check("npm install -g eslint")
	if action != "ask" || pattern != `npm\s+install\s+-g` {
		t.Errorf("Check(npm install -g) = (%s, %q)，期望 ask + 默认模式", action, pattern)
	}
}

// 黑名单优先级不变：Remove-Item -Recurse 命中默认黑名单，仍硬拒而非 ask。
func TestAcceptanceHardening_BlacklistStillBlocks(t *testing.T) {
	filter := hardenedAcceptanceFilter()
	if action, _ := filter.Check("Remove-Item -Recurse foo"); action != "block" {
		t.Errorf("Remove-Item -Recurse 期望 block（黑名单硬拒），实际 %s", action)
	}
}

// 非验收语境行为完全不变：未注入加固名单的过滤器对这些命令保持原判定
// （默认灰名单未覆盖的写命令原本直接放行）。
func TestAcceptanceHardening_BaseFilterUnchanged(t *testing.T) {
	base := NewCommandFilter(DefaultBlacklist, DefaultGreylist)
	for _, cmd := range []string{
		"git add .", "npm install", "rm foo.txt",
		"Set-Content a.txt x", "go mod tidy", "git checkout main",
	} {
		if action, pattern := base.Check(cmd); action != "allow" {
			t.Errorf("base Check(%q) = (%s, %q)，期望 allow（非验收语境行为必须不变）", cmd, action, pattern)
		}
	}
	// 默认灰名单覆盖的命令在两个过滤器上都应是 ask。
	for _, cmd := range []string{"git push origin main", "pip install requests"} {
		if action, _ := base.Check(cmd); action != "ask" {
			t.Errorf("base Check(%q) 期望 ask（默认灰名单原有行为）", cmd)
		}
	}
}

// 派生过滤器与源过滤器共享运行时白名单：任一方向的 allow_session
// 记忆对双方同样生效，保持全局共享单过滤器时期的语义。
func TestDeriveWithExtraGreylist_SharesRuntimeWhitelist(t *testing.T) {
	base := NewCommandFilter(DefaultBlacklist, DefaultGreylist)
	derived := base.DeriveWithExtraGreylist([]string{`^extra-grey$`})

	// 源过滤器上已有的 allow_session 记忆对派生过滤器生效。
	if err := base.AddRuntimeWhitelist(`git\s+push`); err != nil {
		t.Fatal(err)
	}
	if action, _ := derived.Check("git push origin main"); action != "allow" {
		t.Error("派生过滤器应共享源过滤器的 allow_session 记忆")
	}

	// 派生过滤器上新增的 allow_session 记忆对源过滤器生效（白名单短路
	// 先于灰名单匹配，即使模式只存在于派生灰名单）。
	if err := derived.AddRuntimeWhitelist(`^extra-grey$`); err != nil {
		t.Fatal(err)
	}
	if action, _ := base.Check("extra-grey"); action != "allow" {
		t.Error("源过滤器应共享派生过滤器的 allow_session 记忆")
	}
	if !base.IsRuntimeWhitelisted("extra-grey") {
		t.Error("IsRuntimeWhitelisted 应观察到共享白名单")
	}

	// extra 模式只加入派生灰名单：未授权前源过滤器不因 extra 产生 ask。
	base2 := NewCommandFilter(nil, nil)
	derived2 := base2.DeriveWithExtraGreylist([]string{`^extra-grey$`})
	if action, _ := base2.Check("extra-grey"); action != "allow" {
		t.Error("extra 模式不应改变源过滤器的灰名单判定")
	}
	if action, pattern := derived2.Check("extra-grey"); action != "ask" || pattern != `^extra-grey$` {
		t.Errorf("派生过滤器 Check = (%s, %q)，期望 ask + extra 模式", action, pattern)
	}
}
