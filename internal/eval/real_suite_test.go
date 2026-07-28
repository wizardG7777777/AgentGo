package eval

import (
	"os"
	"path/filepath"
	"testing"

	"agentgo/internal/config"
)

// TestLoadRealSuite 守卫仓库根的真实套件（eval/suite.yaml，gitignored）：
// 文件存在时必须能加载且 6 个任务的 prompt 全部内联成功；
// 不存在（如 CI 无此本地资产）则跳过。
func TestLoadRealSuite(t *testing.T) {
	path := filepath.Join("..", "..", "eval", "suite.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("eval/suite.yaml 不存在（本地资产，可能未创建）——跳过")
	}
	s, err := LoadSuite(path)
	if err != nil {
		t.Fatalf("真实套件加载失败: %v", err)
	}
	if len(s.Tasks) != 6 {
		t.Fatalf("真实套件任务数 = %d，期望 6", len(s.Tasks))
	}
	for _, task := range s.Tasks {
		if task.Prompt == "" {
			t.Errorf("任务 %q prompt 为空", task.Name)
		}
		if len(task.Judges) == 0 {
			t.Errorf("任务 %q 无判据", task.Name)
		}
	}
}

// TestRealTemplateValidates 守卫真实配置模板（eval/config.template.yaml）：
// 必须通过 LoadConfig + Validate——模板的字段级缺陷（如 agents 行为参数
// 缺失）曾在首跑时到子进程启动才暴露，空转 90 秒健康等待。
// Validate 会按 cwd 校验 system_prompt_file 存在性，故先切到仓库根。
func TestRealTemplateValidates(t *testing.T) {
	path := filepath.Join("..", "..", "eval", "config.template.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("eval/config.template.yaml 不存在（本地资产）——跳过")
	}
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		t.Fatalf("切换工作目录失败: %v", err)
	}
	cfg, err := config.LoadConfig(filepath.Join("eval", "config.template.yaml"), true)
	if err != nil {
		t.Fatalf("真实模板加载失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("真实模板未通过 v4 校验: %v", err)
	}
}
