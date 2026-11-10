package service

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// FR-13：保存路径上的校验必须拒绝跑不起来的配置。
func TestValidateOCRPipelineParamsRejectsBadValues(t *testing.T) {
	if err := ValidateOCRPipelineParams("manual", map[string]any{
		"manual.render_dpi": float64(300),
	}); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if err := ValidateOCRPipelineParams("manual", map[string]any{
		"manual.render_dpi": float64(601),
	}); err == nil {
		t.Error("out-of-range render_dpi should be rejected")
	}
	if err := ValidateOCRPipelineParams("smart", map[string]any{
		"smart.scan_image_ratio": float64(1.5),
	}); err == nil {
		t.Error("out-of-range scan_image_ratio should be rejected")
	}
	// 未声明的键忽略：旧版本存下的配置在新版本上仍可读。
	if err := ValidateOCRPipelineParams("manual", map[string]any{
		"manual.removed_knob": "whatever",
	}); err != nil {
		t.Errorf("undeclared key should be ignored, got %v", err)
	}
	// 未知流水线 id 按手动流水线校验。
	if err := ValidateOCRPipelineParams("nope", map[string]any{
		"manual.render_dpi": float64(0),
	}); err == nil {
		t.Error("unknown pipeline should still validate against manual fields")
	}
}

func TestValidatePreprocessParams(t *testing.T) {
	if err := ValidatePreprocessParams(map[string]any{
		types.PreprocessParamDenoiseEnabled: true,
		types.PreprocessParamDenoiseSigma:   2.5,
	}); err != nil {
		t.Fatalf("valid preprocess params rejected: %v", err)
	}
	if err := ValidatePreprocessParams(map[string]any{
		types.PreprocessParamDenoiseSigma: 9.0, // 上界 5.0
	}); err == nil {
		t.Error("out-of-range sigma should be rejected")
	}
	if err := ValidatePreprocessParams(map[string]any{
		types.PreprocessParamPad: -1.0, // 下界 0
	}); err == nil {
		t.Error("negative pad should be rejected")
	}
}

// 「开了前处理模式却一个步骤都没启用」在保存时就要被拦下。
func TestBrokenOCRPipelineRules(t *testing.T) {
	broken := BrokenOCRPipelineRules("manual", nil, types.PreprocessModeAlways, nil)
	if len(broken) != 1 {
		t.Fatalf("expected 1 broken rule, got %d", len(broken))
	}
	if broken[0].Field != types.PreprocessModeKey {
		t.Errorf("broken rule should point at %q, got %q", types.PreprocessModeKey, broken[0].Field)
	}

	// 关掉前处理就没有这条约束。
	if got := BrokenOCRPipelineRules("manual", nil, types.PreprocessModeOff, nil); len(got) != 0 {
		t.Errorf("off mode should satisfy rules, got %v", got)
	}
	// 启用任意一个步骤即可。
	if got := BrokenOCRPipelineRules("smart", nil, types.PreprocessModeAuto, map[string]any{
		types.PreprocessParamDeskewEnabled: true,
	}); len(got) != 0 {
		t.Errorf("deskew enabled should satisfy rules, got %v", got)
	}
	// 步骤开关被显式关掉时，规则仍然生效。
	if got := BrokenOCRPipelineRules("smart", nil, types.PreprocessModeAlways, map[string]any{
		types.PreprocessParamDenoiseEnabled:  false,
		types.PreprocessParamDeskewEnabled:   false,
		types.PreprocessParamContrastEnabled: false,
	}); len(got) != 1 {
		t.Errorf("all steps disabled should break the rule, got %v", got)
	}
}

func TestValidateOCRPipelineConfig(t *testing.T) {
	ctx := context.Background()
	if err := ValidateOCRPipelineConfig(ctx, nil); err != nil {
		t.Fatalf("nil cfg should pass: %v", err)
	}
	// 存量知识库（没有 FR-13 字段）必须通过校验。
	if err := ValidateOCRPipelineConfig(ctx, &types.ImageProcessingConfig{ModelID: "m"}); err != nil {
		t.Fatalf("legacy cfg rejected: %v", err)
	}
	if err := ValidateOCRPipelineConfig(ctx, &types.ImageProcessingConfig{
		OCRPipelineID: "smart",
		OCRPipelineParams: map[string]any{
			"smart.heavy_render_dpi": float64(71), // 下界 72
		},
	}); err == nil {
		t.Error("bad pipeline param should be rejected on save")
	}
	if err := ValidateOCRPipelineConfig(ctx, &types.ImageProcessingConfig{
		PreprocessMode:   types.PreprocessModeAlways,
		PreprocessParams: map[string]any{},
	}); err == nil {
		t.Error("preprocess enabled without any step should be rejected on save")
	}
	if err := ValidateOCRPipelineConfig(ctx, &types.ImageProcessingConfig{
		OCRPipelineID:  "smart",
		PreprocessMode: types.PreprocessModeAuto,
		PreprocessParams: map[string]any{
			types.PreprocessParamContrastEnabled: true,
			types.PreprocessParamContrastClip:    1.0,
		},
	}); err != nil {
		t.Errorf("valid smart config rejected: %v", err)
	}
}

// 自描述清单要带「仅对新解析文档生效」的提示，供前端配置页展示（PRD §5.4）。
func TestListOCRPipelinesSelfDescription(t *testing.T) {
	specs := ListOCRPipelines()
	if len(specs) != 2 {
		t.Fatalf("expected 2 pipelines, got %d", len(specs))
	}
	if specs[0].ID != types.OCRPipelineManual {
		t.Errorf("manual pipeline should come first, got %q", specs[0].ID)
	}
	for _, spec := range specs {
		if !strings.Contains(spec.Note, "仅对新解析文档生效") {
			t.Errorf("pipeline %s note missing the new-documents-only hint", spec.ID)
		}
		for _, f := range spec.Fields {
			if f.Label == "" || f.Description == "" {
				t.Errorf("pipeline %s field %s missing label/description", spec.ID, f.Key)
			}
		}
	}
}

// overrides 注入：默认配置下只带两个开关键，其余参数缺席（由 docreader 用
// 它自己的默认值兜底），这样运维的环境变量不会被 Go 侧默认值盖掉。
func TestInjectOCRPipelineOverridesLegacyKB(t *testing.T) {
	ctx := context.Background()
	kb := &types.KnowledgeBase{ImageProcessingConfig: types.ImageProcessingConfig{ModelID: "m"}}
	out := InjectOCRPipelineOverrides(ctx, kb, map[string]string{"pdf_force_scanned": "false"})

	if out[OCRPipelineOverrideKey] != "manual" {
		t.Errorf("pipeline = %q", out[OCRPipelineOverrideKey])
	}
	if out[types.PreprocessModeKey] != types.PreprocessModeOff {
		t.Errorf("preprocess mode = %q", out[types.PreprocessModeKey])
	}
	if _, ok := out["manual.render_dpi"]; ok {
		t.Error("unset params must not be injected, docreader owns those defaults")
	}
	if _, ok := out[types.PreprocessParamPrefix+types.PreprocessParamDenoiseSigma]; ok {
		t.Error("unset preprocess params must not be injected")
	}
	// 既有 override 不能被冲掉。
	if out["pdf_force_scanned"] != "false" {
		t.Error("existing overrides must survive injection")
	}
	// nil kb / nil overrides 都不该 panic。
	if got := InjectOCRPipelineOverrides(ctx, nil, nil); len(got) != 0 {
		t.Errorf("nil kb should produce no keys, got %v", got)
	}
}

func TestInjectOCRPipelineOverridesSmartKB(t *testing.T) {
	ctx := context.Background()
	kb := &types.KnowledgeBase{ImageProcessingConfig: types.ImageProcessingConfig{
		OCRPipelineID: "smart",
		OCRPipelineParams: map[string]any{
			"smart.mixed_use_text_layer":    false,
			"smart.heavy_render_dpi":        float64(400),
			"smart.scan_dense_ratio":        float64(2), // 越界，应被丢弃
			"smart.note.new_documents_only": "x",        // note 字段不下发
			"manual.force_scanned":          true,       // 别的流水线的残留参数不下发
		},
		PreprocessMode: types.PreprocessModeAlways,
		PreprocessParams: map[string]any{
			types.PreprocessParamDenoiseEnabled: true,
			types.PreprocessParamDenoiseSigma:   float64(2.5),
			types.PreprocessParamContrastClip:   float64(99), // 越界，应被丢弃
		},
	}}

	out := InjectOCRPipelineOverrides(ctx, kb, nil)
	if out[OCRPipelineOverrideKey] != "smart" {
		t.Errorf("pipeline = %q", out[OCRPipelineOverrideKey])
	}
	if out[types.PreprocessModeKey] != types.PreprocessModeAlways {
		t.Errorf("preprocess mode = %q", out[types.PreprocessModeKey])
	}
	if out["smart.mixed_use_text_layer"] != "false" {
		t.Errorf("bool param = %q", out["smart.mixed_use_text_layer"])
	}
	if out["smart.heavy_render_dpi"] != "400" {
		t.Errorf("int param = %q", out["smart.heavy_render_dpi"])
	}
	if out[types.PreprocessParamPrefix+types.PreprocessParamDenoiseEnabled] != "true" {
		t.Errorf("preprocess switch = %q", out[types.PreprocessParamPrefix+types.PreprocessParamDenoiseEnabled])
	}
	if out[types.PreprocessParamPrefix+types.PreprocessParamDenoiseSigma] != "2.5" {
		t.Errorf("preprocess sigma = %q", out[types.PreprocessParamPrefix+types.PreprocessParamDenoiseSigma])
	}
	for _, dropped := range []string{
		"smart.scan_dense_ratio",
		"manual.force_scanned",
		"smart.note.new_documents_only",
		types.PreprocessParamPrefix + types.PreprocessParamContrastClip,
	} {
		if _, ok := out[dropped]; ok {
			t.Errorf("%q should not be injected", dropped)
		}
	}
}

// 存量兼容（PRD §5.4）：OCR 配置不得进入重解析白名单 —— 改开关不动存量文档。
func TestOCRPipelineConfigIsNotAReparseTrigger(t *testing.T) {
	k := &types.Knowledge{FileType: "pdf", FileName: "a.pdf"}
	got := reparseFileTypes(k)
	if len(got) != 1 || got[0] != "pdf" {
		t.Fatalf("reparseFileTypes = %v", got)
	}
	// reparseFileTypes 只看文件类型，与 OCR 流水线配置无关：
	// 同一份文档在两种流水线配置下得到的白名单必须完全一致。
	kbA := &types.KnowledgeBase{ImageProcessingConfig: types.ImageProcessingConfig{OCRPipelineID: "manual"}}
	kbB := &types.KnowledgeBase{ImageProcessingConfig: types.ImageProcessingConfig{
		OCRPipelineID:    "smart",
		PreprocessMode:   types.PreprocessModeAlways,
		PreprocessParams: map[string]any{types.PreprocessParamDenoiseEnabled: true},
	}}
	kbA.ImageProcessingConfig.EnsureOCRDefaults()
	kbB.ImageProcessingConfig.EnsureOCRDefaults()
	if !equalStringSlices(reparseFileTypes(k), got) {
		t.Error("reparse whitelist must not depend on OCR config")
	}
	if kbA.ImageProcessingConfig.OCRPipelineID == kbB.ImageProcessingConfig.OCRPipelineID {
		t.Error("sanity: the two KBs should differ in pipeline id")
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
