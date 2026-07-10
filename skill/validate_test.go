package skill

import "testing"

// TestValidatePlaceholdersEvidenceSyntax:自定义 rewoo 阶段提示词里的证据
// 引用语法 {e1}/{eN} 是指导语不是占位符,P4 校验必须放行(实测曾误拦)。
func TestValidatePlaceholdersEvidenceSyntax(t *testing.T) {
	custom := `规划器。步骤间用 {e1} 引用前序结果,形如 {eN}。`
	if err := validatePlaceholders("skill x engine_config.planner", custom, nil); err != nil {
		t.Errorf("合法 rewoo 证据语法被误拦: %v", err)
	}
	// 但普通未声明占位符仍必须拦(豁免不能开成后门)
	if err := validatePlaceholders("skill x prompt", "分析 {skuu}", nil); err == nil {
		t.Error("未声明占位符 {skuu} 必须报错")
	}
}
