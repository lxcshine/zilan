package types

import (
	"strings"
	"testing"
)

// FR-13：流水线 id 的规范化 —— 空值与未知值都必须回落到手动流水线，
// 这样知识库里存着一个已下线的 id 也不会让解析失败。
func TestNormalizeOCRPipelineID(t *testing.T) {
	cases := []struct {
		in   OCRPipelineID
		want OCRPipelineID
	}{
		{"", OCRPipelineManual},
		{"manual", OCRPipelineManual},
		{"MANUAL", OCRPipelineManual},
		{" smart ", OCRPipelineSmart},
		{"smartocr", OCRPipelineManual}, // 上游的旧名在 Zilan 不存在，回落手动
		{"nonsense", OCRPipelineManual},
	}
	for _, c := range cases {
		if got := NormalizeOCRPipelineID(c.in); got != c.want {
			t.Errorf("NormalizeOCRPipelineID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if !IsKnownOCRPipelineID("smart") || IsKnownOCRPipelineID("nope") {
		t.Error("IsKnownOCRPipelineID mismatch")
	}
}

func TestSlotKeyNamespacesParamKeys(t *testing.T) {
	if got := SlotKey("manual", "render_dpi"); got != "manual.render_dpi" {
		t.Errorf("SlotKey = %q", got)
	}
	// 已经带前缀的键不会被二次加前缀。
	if got := SlotKey("manual", "manual.render_dpi"); got != "manual.render_dpi" {
		t.Errorf("SlotKey double prefix = %q", got)
	}
	if got := SlotKey("", "render_dpi"); got != "render_dpi" {
		t.Errorf("SlotKey empty pipeline = %q", got)
	}
}

func TestOCRPipelineParamReadsSlotThenBareKey(t *testing.T) {
	params := map[string]any{
		"smart.scan_dense_ratio": 0.8,
		"heavy_render_dpi":       400,
		"nil_key":                nil,
	}
	if v, ok := OCRPipelineParam(params, "smart", "scan_dense_ratio"); !ok || v != 0.8 {
		t.Errorf("slot key lookup = %v, %v", v, ok)
	}
	// 兼容裸键写法。
	if v, ok := OCRPipelineParam(params, "smart", "heavy_render_dpi"); !ok || v != 400 {
		t.Errorf("bare key lookup = %v, %v", v, ok)
	}
	// nil 值等同于「没配过」，调用方应回落默认值。
	if _, ok := OCRPipelineParam(params, "smart", "nil_key"); ok {
		t.Error("nil value should be reported as absent")
	}
	if _, ok := OCRPipelineParam(nil, "smart", "anything"); ok {
		t.Error("nil params should be reported as absent")
	}
}

// 存量兼容（PRD §5.4）：FR-13 之前创建的知识库没有这些字段，
// 解析出来必须是「手动流水线 + 前处理关闭」。
func TestResolveDefaultsForLegacyConfig(t *testing.T) {
	if got := ResolveOCRPipelineID(nil); got != OCRPipelineManual {
		t.Errorf("nil cfg pipeline = %q", got)
	}
	if got := ResolvePreprocessMode(nil); got != PreprocessModeOff {
		t.Errorf("nil cfg preprocess = %q", got)
	}
	legacy := &ImageProcessingConfig{ModelID: "vlm-1"}
	if got := ResolveOCRPipelineID(legacy); got != OCRPipelineManual {
		t.Errorf("legacy pipeline = %q", got)
	}
	if got := ResolvePreprocessMode(legacy); got != PreprocessModeOff {
		t.Errorf("legacy preprocess = %q", got)
	}
	if got := legacy.ResolveOCRPipeline(); got != OCRPipelineManual {
		t.Errorf("value receiver pipeline = %q", got)
	}

	cfg := &ImageProcessingConfig{OCRPipelineID: "smart", PreprocessMode: "AUTO"}
	if got := ResolveOCRPipelineID(cfg); got != OCRPipelineSmart {
		t.Errorf("smart pipeline = %q", got)
	}
	if got := ResolvePreprocessMode(cfg); got != PreprocessModeAuto {
		t.Errorf("preprocess mode = %q", got)
	}
}

// 模式也可能被写进流水线参数表（面板上它是流水线的一个字段）。
func TestResolvePreprocessModeFromPipelineParams(t *testing.T) {
	cfg := &ImageProcessingConfig{
		OCRPipelineID:     "smart",
		OCRPipelineParams: map[string]any{SlotKey("smart", PreprocessModeKey): "always"},
	}
	if got := ResolvePreprocessMode(cfg); got != PreprocessModeAlways {
		t.Errorf("preprocess mode from params = %q", got)
	}
}

func TestNormalizePreprocessMode(t *testing.T) {
	cases := map[string]string{
		"":         PreprocessModeOff,
		" Off ":    PreprocessModeOff,
		"auto":     PreprocessModeAuto,
		"ALWAYS":   PreprocessModeAlways,
		"whatever": PreprocessModeOff,
	}
	for in, want := range cases {
		if got := NormalizePreprocessMode(in); got != want {
			t.Errorf("NormalizePreprocessMode(%q) = %q, want %q", in, got, want)
		}
	}
	if !PreprocessRunsUnconditionally("always") || PreprocessRunsUnconditionally("auto") {
		t.Error("PreprocessRunsUnconditionally mismatch")
	}
	if !PreprocessObservesGeometry("auto") || PreprocessObservesGeometry("always") {
		t.Error("PreprocessObservesGeometry mismatch")
	}
}

func TestEnsureOCRDefaultsOnlyOnSavePath(t *testing.T) {
	cfg := &ImageProcessingConfig{}
	cfg.EnsureOCRDefaults()
	if cfg.OCRPipelineID != "manual" || cfg.PreprocessMode != PreprocessModeOff {
		t.Errorf("EnsureOCRDefaults = %+v", cfg)
	}
	cfg = &ImageProcessingConfig{OCRPipelineID: "gone", PreprocessMode: "weird"}
	cfg.EnsureOCRDefaults()
	if cfg.OCRPipelineID != "manual" || cfg.PreprocessMode != PreprocessModeOff {
		t.Errorf("EnsureOCRDefaults normalization = %+v", cfg)
	}
	var nilCfg *ImageProcessingConfig
	nilCfg.EnsureOCRDefaults() // 不应 panic
}

// 取值校验：类型不对或越界都必须被拒绝（而不是 clamp 到边界）。
func TestValidateOCRPipelineFieldValue(t *testing.T) {
	dpiField := OCRFieldDef{
		Key: "render_dpi", Type: OCRFieldTypeInt, Default: 200,
		Min: ocrBound(72), Max: ocrBound(600),
	}
	if v, err := ValidateOCRPipelineFieldValue(dpiField, float64(300)); err != nil || v != 300 {
		t.Errorf("valid int = %v, %v", v, err)
	}
	// JSON 里的数字是 float64，字符串数字也要能读。
	if v, err := ValidateOCRPipelineFieldValue(dpiField, "300"); err != nil || v != 300 {
		t.Errorf("string int = %v, %v", v, err)
	}
	if _, err := ValidateOCRPipelineFieldValue(dpiField, float64(601)); err == nil {
		t.Error("out-of-range int should be rejected")
	} else if !strings.Contains(err.Error(), "render_dpi") {
		t.Errorf("error should name the field: %v", err)
	}
	if _, err := ValidateOCRPipelineFieldValue(dpiField, "abc"); err == nil {
		t.Error("non-numeric should be rejected")
	}

	boolField := OCRFieldDef{Key: "force_scanned", Type: OCRFieldTypeBool, Default: false}
	if v, err := ValidateOCRPipelineFieldValue(boolField, "true"); err != nil || v != true {
		t.Errorf("bool from string = %v, %v", v, err)
	}
	if _, err := ValidateOCRPipelineFieldValue(boolField, "maybe"); err == nil {
		t.Error("invalid bool should be rejected")
	}

	enumField := preprocessModeField()
	if v, err := ValidateOCRPipelineFieldValue(enumField, "ALWAYS"); err != nil || v != PreprocessModeAlways {
		t.Errorf("enum normalize = %v, %v", v, err)
	}
	if _, err := ValidateOCRPipelineFieldValue(enumField, "sometimes"); err == nil {
		t.Error("unknown enum should be rejected")
	}

	// note 字段不承载取值，直接通过且不产生值。
	noteField := OCRFieldDef{Key: "note.x", Type: OCRFieldTypeNote}
	if v, err := ValidateOCRPipelineFieldValue(noteField, "anything"); err != nil || v != nil {
		t.Errorf("note field = %v, %v", v, err)
	}
}

func TestSanitizeOCRPipelineParamsRejectsAndKeeps(t *testing.T) {
	fields := OCRPipelineFields(OCRPipelineManual)
	params := map[string]any{
		"manual.render_dpi":   float64(300), // 合法
		"manual.jpeg_quality": float64(999), // 越界
		"future.unknown":      "keepme",     // 未声明的键原样保留（向前兼容）
	}
	out, rejected := SanitizeOCRPipelineParams(fields, params, "manual")
	if out["manual.render_dpi"] != 300 {
		t.Errorf("valid param dropped: %v", out)
	}
	if _, ok := out["manual.jpeg_quality"]; ok {
		t.Error("out-of-range param should be dropped, not clamped")
	}
	if len(rejected) != 1 || rejected[0] != "manual.jpeg_quality" {
		t.Errorf("rejected = %v", rejected)
	}
	if out["future.unknown"] != "keepme" {
		t.Error("undeclared keys must survive for forward compatibility")
	}
}

// PRD 要求前处理默认关闭：所有开关字段的默认值都必须是 false。
func TestPreprocessDefaultsAreAllDisabled(t *testing.T) {
	defaults := DefaultPreprocessParams()
	for _, key := range []string{
		PreprocessParamDenoiseEnabled,
		PreprocessParamDeskewEnabled,
		PreprocessParamContrastEnabled,
	} {
		if v, ok := defaults[key].(bool); !ok || v {
			t.Errorf("%s default = %v, want false", key, defaults[key])
		}
	}
	// 数值参数的默认值必须落在声明范围内。
	for _, f := range PreprocessFields() {
		if _, err := ValidateOCRPipelineFieldValue(f, f.Default); err != nil {
			t.Errorf("field %s default %v invalid: %v", f.Key, f.Default, err)
		}
	}
}

func TestDefaultOCRPipelineParamsUsesSlotKeys(t *testing.T) {
	params := DefaultOCRPipelineParams(OCRPipelineSmart)
	if _, ok := params["smart.mixed_use_text_layer"]; !ok {
		t.Errorf("expected slot keys, got %v", params)
	}
	if _, ok := params["smart.note.new_documents_only"]; ok {
		t.Error("note fields must not produce a value")
	}
	for _, f := range OCRPipelineFields(OCRPipelineManual) {
		if f.Type == OCRFieldTypeNote {
			continue
		}
		if _, err := ValidateOCRPipelineFieldValue(f, f.Default); err != nil {
			t.Errorf("manual field %s default invalid: %v", f.Key, err)
		}
	}
}

// 存量兼容提示（PRD §5.4）必须出现在自描述里，供前端配置页展示。
func TestOCRPipelineSpecsCarryNewDocumentsOnlyNote(t *testing.T) {
	specs := OCRPipelineSpecs()
	if len(specs) != 2 {
		t.Fatalf("expected 2 pipelines, got %d", len(specs))
	}
	for _, spec := range specs {
		if !strings.Contains(spec.Note, "仅对新解析文档生效") {
			t.Errorf("pipeline %s missing new-documents-only note: %q", spec.ID, spec.Note)
		}
		if !strings.Contains(spec.Note, "手动重新解析") {
			t.Errorf("pipeline %s note should mention manual reparse: %q", spec.ID, spec.Note)
		}
		if len(spec.Fields) == 0 {
			t.Errorf("pipeline %s declares no fields", spec.ID)
		}
		foundNote := false
		for _, f := range spec.Fields {
			if f.Type == OCRFieldTypeNote && strings.Contains(f.Description, "仅对新解析文档生效") {
				foundNote = true
			}
		}
		if !foundNote {
			t.Errorf("pipeline %s should expose the note as a field for the config panel", spec.ID)
		}
	}
	// 未知 id 回落手动流水线的声明。
	if OCRPipelineSpecFor("nope").ID != OCRPipelineManual {
		t.Error("unknown pipeline should fall back to manual spec")
	}
}

// 前处理规则必须挂在两条流水线上，否则「开了前处理却一个步骤都没开」不会被拦下。
func TestBothPipelinesDeclarePreprocessRule(t *testing.T) {
	for _, id := range OCRPipelineIDs {
		rules := OCRPipelineRulesFor(id)
		if len(rules) == 0 {
			t.Fatalf("pipeline %s declares no rules", id)
		}
		found := false
		for _, r := range rules {
			if r.WhenField == PreprocessModeKey && len(r.AtLeastOne) == 3 {
				found = true
			}
		}
		if !found {
			t.Errorf("pipeline %s missing the preprocess at-least-one rule", id)
		}
	}
}
