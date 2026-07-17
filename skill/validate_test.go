package skill

import "testing"

// TestValidatePlaceholders:P4 占位符声明校验——未声明占位符必须装配期
// 报错,内置变量放行。
func TestValidatePlaceholders(t *testing.T) {
	if err := validatePlaceholders("skill x prompt", "分析 {skuu}", nil); err == nil {
		t.Error("未声明占位符 {skuu} 必须报错")
	}
	if err := validatePlaceholders("skill x prompt", "原始诉求 {$user_input},输入 {$input},用户 {$user_id}", nil); err != nil {
		t.Errorf("内置变量必须放行: %v", err)
	}
}
