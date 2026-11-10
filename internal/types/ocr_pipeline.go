package types

// 本文件是 OCR 流水线层（FR-13）的词汇表：流水线 id、每个流水线可暴露的
// 可调参数声明、以及参数键的命名空间规则。参数「怎么跑」由 docreader
// （Python）与 application/service 绑定，这里只描述「有哪些旋钮、默认值是
// 什么、取值范围是什么」，因此本包不引入任何传输层依赖。
//
// 存量兼容（PRD §5.4）：所有新增字段都挂在 KnowledgeBase 既有的 JSON 列
// image_processing_config 上，不需要数据库迁移；未设置时一律回落到
// 「手动流水线 + 前处理关闭」，与 FR-13 之前的行为完全一致。配置变更也
// 不会触发存量文档重解析（见 reparseFileTypes 白名单，未收录本项配置）。

import (
	"encoding/json"
	"strconv"
	"strings"
)

// OCRPipelineID 是一条 OCR 流水线的标识。
type OCRPipelineID string

const (
	// OCRPipelineManual 手动流水线：按固定参数执行，不做页面分类路由。
	OCRPipelineManual OCRPipelineID = "manual"
	// OCRPipelineSmart 智能流水线：先做页面分类（文本页/扫描页/图片页/
	// 混合页），再按页类型选择不同的处理路径。
	OCRPipelineSmart OCRPipelineID = "smart"
)

// OCRPipelineIDs 列出所有已注册的流水线 id，顺序稳定（供前端下拉渲染）。
var OCRPipelineIDs = []OCRPipelineID{OCRPipelineManual, OCRPipelineSmart}

// IsKnownOCRPipelineID 报告一个字符串是否是已注册的流水线 id。
func IsKnownOCRPipelineID(id string) bool {
	for _, known := range OCRPipelineIDs {
		if string(known) == id {
			return true
		}
	}
	return false
}

// NormalizeOCRPipelineID 把任意输入规范化为一个可用的流水线 id。
// 空值与未知值一律回落到手动流水线：知识库里存着一个本版本已下线的 id
// 时，静默降级比让文档解析失败更安全。
func NormalizeOCRPipelineID(id OCRPipelineID) OCRPipelineID {
	trimmed := strings.ToLower(strings.TrimSpace(string(id)))
	switch OCRPipelineID(trimmed) {
	case OCRPipelineSmart:
		return OCRPipelineSmart
	case OCRPipelineManual:
		return OCRPipelineManual
	default:
		return OCRPipelineManual
	}
}

// String 让 OCRPipelineID 可以直接用于日志与 map 键。
func (id OCRPipelineID) String() string { return string(id) }

// OCRFieldType 是参数在前端渲染成的控件类型。
type OCRFieldType string

const (
	// OCRFieldTypeBool 渲染成开关。
	OCRFieldTypeBool OCRFieldType = "bool"
	// OCRFieldTypeInt 渲染成整数输入框。
	OCRFieldTypeInt OCRFieldType = "int"
	// OCRFieldTypeFloat 渲染成小数输入框。
	OCRFieldTypeFloat OCRFieldType = "float"
	// OCRFieldTypeEnum 渲染成下拉框，取值限定在 Options 内。
	OCRFieldTypeEnum OCRFieldType = "enum"
	// OCRFieldTypeNote 不渲染控件，只是一段说明文字。
	OCRFieldTypeNote OCRFieldType = "note"
)

// OCRFieldGroup 是参数在配置面板里的分组。
type OCRFieldGroup string

const (
	// OCRFieldGroupBasic 常驻开关，直接展示。
	OCRFieldGroupBasic OCRFieldGroup = "basic"
	// OCRFieldGroupAdvanced 折叠在「高级设置」里。
	OCRFieldGroupAdvanced OCRFieldGroup = "advanced"
	// OCRFieldGroupNote 折叠在「说明」里。
	OCRFieldGroupNote OCRFieldGroup = "note"
)

// OCRFieldDef 是一个可调参数的自描述声明。前端凭它渲染控件、显示默认值
// 与取值范围，后端凭它做校验，二者读同一份声明，因此新增一个旋钮不需要
// 前端改代码。
type OCRFieldDef struct {
	// Key 是参数的裸名（不带流水线前缀）。存储与传输时用 SlotKey 加前缀。
	Key string `json:"key"`
	// Type 决定前端控件类型，同时决定后端的类型校验规则。
	Type OCRFieldType `json:"type"`
	// Label / Description 使用项目默认语言书写，前端可叠加翻译并回落到此。
	Label       string `json:"label"`
	Description string `json:"description"`
	// Default 在知识库没有覆盖该参数时生效，也是面板上显示的初始值。
	Default any `json:"default"`
	// Min / Max 是数值型参数的取值范围（闭区间）。nil 表示该方向无界。
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// Options 枚举 enum 型参数的全部合法取值。
	Options []string `json:"options,omitempty"`
	// Group 决定参数落在面板的哪一块。
	Group OCRFieldGroup `json:"group,omitempty"`
}

// OCRPipelineRules 是流水线对自身参数声明的一条约束。目前只支持一种形状：
// 「当 WhenField 取到 WhenValues 中的某个值时，AtLeastOne 里至少要有一个为真」。
// 规则声明在这里而不是写死在校验函数里，前端可以据此在用户提交前就给出提示。
type OCRPipelineRules struct {
	// AtLeastOne 列出至少需要一个为 true 的布尔参数键（裸名）。
	AtLeastOne []string `json:"at_least_one,omitempty"`
	// WhenField 是规则的生效条件字段；为空表示规则恒定生效。
	WhenField string `json:"when_field,omitempty"`
	// WhenValues 列出使规则生效的 WhenField 取值；为空表示任意非空值都生效。
	WhenValues []string `json:"when_values,omitempty"`
	// MessageKey 是前端翻译用的文案键。
	MessageKey string `json:"message_key,omitempty"`
	// Field 指向应当修改的那个控件；为空时取 AtLeastOne 的第一项。
	Field string `json:"field,omitempty"`
}

// OCRPipelineSpec 是一条流水线在配置面板眼里的完整样子。
type OCRPipelineSpec struct {
	ID          OCRPipelineID `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	// Note 是面板上的常驻提示文案，FR-13 用它承载「仅对新解析文档生效」。
	Note   string             `json:"note"`
	Fields []OCRFieldDef      `json:"fields"`
	Rules  []OCRPipelineRules `json:"rules,omitempty"`
}

// OCRPipelineValidationError 是一条被违反的规则，实现了 error 接口，因此
// 校验函数可以直接把它当作错误返回，同时保留结构化的字段指向。
type OCRPipelineValidationError struct {
	MessageKey string `json:"message_key"`
	Field      string `json:"field"`
	Message    string `json:"message"`
}

func (e *OCRPipelineValidationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.MessageKey
}

// OCROnlyNewDocumentsNote 是配置面板必须展示的存量兼容提示（PRD §5.4）：
// OCR 流水线与前处理开关只影响此后新解析的文档，已经入库的文档不会因为
// 开关变更被自动重解析。
const OCROnlyNewDocumentsNote = "OCR 流水线与前处理配置仅对新解析文档生效；存量文档不会自动重解析，如需应用新配置请手动重新解析。"

// 前处理模式的三个取值。默认 off，符合 PRD「OCR 前处理默认关闭」的要求。
//
//	auto   由解析侧按页面情况自行判断是否值得前处理（当前实现：对渲染出的
//	       页面图统一尝试，判定不足则原样返回）；
//	off    （默认）完全不做前处理，行为与 FR-13 之前一致；
//	always 对每一张渲染出的页面图都执行已启用的前处理步骤。
const (
	PreprocessModeOff    = "off"
	PreprocessModeAuto   = "auto"
	PreprocessModeAlways = "always"
)

// PreprocessModeKey 是前处理模式开关的参数名。与其它参数一样，它存储在
// 所属流水线的 slot 下。
const PreprocessModeKey = "preprocess.mode"

// PreprocessParamPrefix 是前处理参数在跨进程传输（parser_engine_overrides）
// 时统一加上的前缀，避免与流水线参数、既有 override 键撞名。
const PreprocessParamPrefix = "preprocess."

// NormalizePreprocessMode 把任意输入规范化为一个合法的前处理模式。
// 未知值与空值一律回落到 off（默认关闭）。
func NormalizePreprocessMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case PreprocessModeAuto:
		return PreprocessModeAuto
	case PreprocessModeAlways:
		return PreprocessModeAlways
	default:
		return PreprocessModeOff
	}
}

// PreprocessRunsUnconditionally 报告该模式是否对每一页都执行前处理。
func PreprocessRunsUnconditionally(mode string) bool {
	return NormalizePreprocessMode(mode) == PreprocessModeAlways
}

// PreprocessObservesGeometry 报告该模式是否由解析侧自行判断是否前处理。
func PreprocessObservesGeometry(mode string) bool {
	return NormalizePreprocessMode(mode) == PreprocessModeAuto
}

// SlotKey 用一个流水线 id 给参数键加命名空间，得到「<pipeline>.<key>」。
// 已经带该前缀的键不会被二次加前缀，空流水线 id 原样返回。
func SlotKey(pipelineID, key string) string {
	if pipelineID == "" || strings.HasPrefix(key, pipelineID+".") {
		return key
	}
	return pipelineID + "." + key
}

// OCRPipelineParam 从参数表里读出一个流水线参数。找不到（或值为 nil）说明
// 知识库从未存过这一项，调用方应当回落到字段声明的默认值 —— 与配置面板
// 显示的答案一致。
func OCRPipelineParam(params map[string]any, pipelineID, key string) (any, bool) {
	if params == nil {
		return nil, false
	}
	if v, ok := params[SlotKey(pipelineID, key)]; ok && v != nil {
		return v, true
	}
	// 兼容裸键写法（旧数据或前端未加前缀）。
	if v, ok := params[key]; ok && v != nil {
		return v, true
	}
	return nil, false
}

// ResolveOCRPipelineID 返回知识库请求的流水线 id。未设置或未知一律回落到
// 手动流水线，因此升级前创建的知识库行为不变。
func ResolveOCRPipelineID(cfg *ImageProcessingConfig) OCRPipelineID {
	if cfg == nil {
		return OCRPipelineManual
	}
	return NormalizeOCRPipelineID(OCRPipelineID(cfg.OCRPipelineID))
}

// ResolvePreprocessMode 返回知识库请求的前处理模式，未设置回落 off。
func ResolvePreprocessMode(cfg *ImageProcessingConfig) string {
	if cfg == nil {
		return PreprocessModeOff
	}
	if m := strings.TrimSpace(cfg.PreprocessMode); m != "" {
		return NormalizePreprocessMode(m)
	}
	// 兼容：模式也可能被写进流水线参数表（面板上它就是一个流水线字段）。
	if v, ok := OCRPipelineParam(cfg.OCRPipelineParams, ResolveOCRPipelineID(cfg).String(), PreprocessModeKey); ok {
		if s, ok2 := v.(string); ok2 {
			return NormalizePreprocessMode(s)
		}
	}
	return PreprocessModeOff
}

// --- 参数声明 ---------------------------------------------------------------

func ocrBound(v float64) *float64 { return &v }

// preprocessModeField 是两条流水线共享的前处理模式开关声明。
func preprocessModeField() OCRFieldDef {
	return OCRFieldDef{
		Key:         PreprocessModeKey,
		Type:        OCRFieldTypeEnum,
		Label:       "OCR 前处理",
		Description: "在 OCR 之前对渲染出的页面图做去噪/纠偏/对比度增强。默认关闭。",
		Default:     PreprocessModeOff,
		Options:     []string{PreprocessModeOff, PreprocessModeAuto, PreprocessModeAlways},
		Group:       OCRFieldGroupBasic,
	}
}

// preprocessRule 声明「开启了前处理模式，就必须至少启用一个前处理步骤」。
// 该规则的 AtLeastOne 键位于前处理参数表（PreprocessParams）而非流水线参数表，
// 求值前需要把两张表合并（见 service.BrokenOCRPipelineRules）。
func preprocessRule() OCRPipelineRules {
	return OCRPipelineRules{
		AtLeastOne: []string{
			PreprocessParamDenoiseEnabled,
			PreprocessParamDeskewEnabled,
			PreprocessParamContrastEnabled,
		},
		WhenField:  PreprocessModeKey,
		WhenValues: []string{PreprocessModeAuto, PreprocessModeAlways},
		MessageKey: "ocr.preprocess.no_step_enabled",
		Field:      PreprocessModeKey,
	}
}

// 前处理参数键（裸名，存储于 ImageProcessingConfig.PreprocessParams）。
const (
	PreprocessParamDenoiseEnabled  = "denoise.enabled"
	PreprocessParamDenoiseSigma    = "denoise.sigma"
	PreprocessParamDeskewEnabled   = "deskew.enabled"
	PreprocessParamDeskewAngTol    = "deskew.ang_tol"
	PreprocessParamDeskewMinScore  = "deskew.min_score"
	PreprocessParamContrastEnabled = "contrast.enabled"
	PreprocessParamContrastClip    = "contrast.clip"
	PreprocessParamPad             = "pad"
)

// 手动流水线：按固定参数执行，所有页走同一条路径。
var manualOCRPipelineFields = []OCRFieldDef{
	{
		Key:         "force_scanned",
		Type:        OCRFieldTypeBool,
		Label:       "全部按扫描页处理",
		Description: "忽略文本层，把每一页都渲染成图片交给 OCR/VLM。适用于文本层缺失或不可信的 PDF。",
		Default:     false,
		Group:       OCRFieldGroupBasic,
	},
	{
		Key:         "render_dpi",
		Type:        OCRFieldTypeInt,
		Label:       "渲染 DPI",
		Description: "把页面渲染成图片时使用的分辨率。越高越清晰，但耗时与体积也越大。",
		Default:     200,
		Min:         ocrBound(72),
		Max:         ocrBound(600),
		Group:       OCRFieldGroupAdvanced,
	},
	{
		Key:         "jpeg_quality",
		Type:        OCRFieldTypeInt,
		Label:       "JPEG 质量",
		Description: "渲染出的页面图压缩质量（1-95）。",
		Default:     85,
		Min:         ocrBound(1),
		Max:         ocrBound(95),
		Group:       OCRFieldGroupAdvanced,
	},
	preprocessModeField(),
	{
		Key:         "note.new_documents_only",
		Type:        OCRFieldTypeNote,
		Label:       "生效范围",
		Description: OCROnlyNewDocumentsNote,
		Group:       OCRFieldGroupNote,
	},
}

// 智能流水线：先分类再按页类型选路径。
var smartOCRPipelineFields = []OCRFieldDef{
	{
		Key:         "scan_image_ratio",
		Type:        OCRFieldTypeFloat,
		Label:       "图像面积占比阈值",
		Description: "页面上图片对象覆盖面积达到该比例时，判定为图片主导页（图片页/混合页）。原生数字页通常约 0.0-0.05，扫描页约 1.0。",
		Default:     0.5,
		Min:         ocrBound(0),
		Max:         ocrBound(1),
		Group:       OCRFieldGroupAdvanced,
	},
	{
		Key:         "min_chars_per_page",
		Type:        OCRFieldTypeInt,
		Label:       "文本层字符数阈值",
		Description: "低于该字符数认为页面没有可用文本层。与图像面积占比共同决定页面归入文本页/扫描页/图片页/混合页。",
		Default:     10,
		Min:         ocrBound(0),
		Max:         ocrBound(2000),
		Group:       OCRFieldGroupAdvanced,
	},
	{
		Key:         "mixed_use_text_layer",
		Type:        OCRFieldTypeBool,
		Label:       "混合页保留文本层",
		Description: "混合页（图片主导但存在可用文本层）同时保留文本层并渲染整页图。关闭后混合页与图片页同等处理，只渲染不取文本层。",
		Default:     true,
		Group:       OCRFieldGroupBasic,
	},
	{
		Key:         "scan_dense_ratio",
		Type:        OCRFieldTypeFloat,
		Label:       "扫描密集判定比例",
		Description: "需要渲染的页面占比达到该值时，判定为扫描密集文档，自动提高渲染 DPI。",
		Default:     0.5,
		Min:         ocrBound(0),
		Max:         ocrBound(1),
		Group:       OCRFieldGroupAdvanced,
	},
	{
		Key:         "heavy_render_dpi",
		Type:        OCRFieldTypeInt,
		Label:       "扫描密集渲染 DPI",
		Description: "扫描密集文档使用的渲染分辨率（仅在高于服务默认 DPI 时生效）。",
		Default:     300,
		Min:         ocrBound(72),
		Max:         ocrBound(600),
		Group:       OCRFieldGroupAdvanced,
	},
	preprocessModeField(),
	{
		Key:         "note.new_documents_only",
		Type:        OCRFieldTypeNote,
		Label:       "生效范围",
		Description: OCROnlyNewDocumentsNote,
		Group:       OCRFieldGroupNote,
	},
}

// PreprocessFields 声明全部 OCR 前处理参数。默认全部关闭（PRD 要求）。
func PreprocessFields() []OCRFieldDef {
	return []OCRFieldDef{
		{
			Key:         PreprocessParamDenoiseEnabled,
			Type:        OCRFieldTypeBool,
			Label:       "去噪",
			Description: "中值滤波抑制扫描噪点与 JPEG 压缩块，强度由 sigma 控制。默认关闭。",
			Default:     false,
			Group:       OCRFieldGroupBasic,
		},
		{
			Key:         PreprocessParamDenoiseSigma,
			Type:        OCRFieldTypeFloat,
			Label:       "去噪强度 sigma",
			Description: "越大去噪越强、细节损失越多。取值 0.1-5.0。",
			Default:     1.0,
			Min:         ocrBound(0.1),
			Max:         ocrBound(5.0),
			Group:       OCRFieldGroupAdvanced,
		},
		{
			Key:         PreprocessParamDeskewEnabled,
			Type:        OCRFieldTypeBool,
			Label:       "纠偏",
			Description: "按投影轮廓估计倾斜角并旋正。需要 numpy，缺失时自动跳过。默认关闭。",
			Default:     false,
			Group:       OCRFieldGroupBasic,
		},
		{
			Key:         PreprocessParamDeskewAngTol,
			Type:        OCRFieldTypeFloat,
			Label:       "纠偏最大角度",
			Description: "只在该角度范围内搜索倾斜角（度）。取值 0-90。",
			Default:     15.0,
			Min:         ocrBound(0),
			Max:         ocrBound(90),
			Group:       OCRFieldGroupAdvanced,
		},
		{
			Key:         PreprocessParamDeskewMinScore,
			Type:        OCRFieldTypeFloat,
			Label:       "纠偏置信度阈值",
			Description: "估计结果的置信度低于该值时不旋转，避免把本来正的页面转歪。取值 0-1。",
			Default:     0.5,
			Min:         ocrBound(0),
			Max:         ocrBound(1),
			Group:       OCRFieldGroupAdvanced,
		},
		{
			Key:         PreprocessParamContrastEnabled,
			Type:        OCRFieldTypeBool,
			Label:       "对比度增强",
			Description: "自动色阶拉伸，改善偏灰扫描件的可读性。默认关闭。",
			Default:     false,
			Group:       OCRFieldGroupBasic,
		},
		{
			Key:         PreprocessParamContrastClip,
			Type:        OCRFieldTypeFloat,
			Label:       "对比度裁剪比例",
			Description: "色阶拉伸时两端各裁掉的百分比，抑制离群像素。取值 0-5。",
			Default:     2.0,
			Min:         ocrBound(0),
			Max:         ocrBound(5),
			Group:       OCRFieldGroupAdvanced,
		},
		{
			Key:         PreprocessParamPad,
			Type:        OCRFieldTypeFloat,
			Label:       "页面留白比例",
			Description: "在图片四周补白边的比例（相对短边），帮助 OCR 处理贴边内容。取值 0-0.2；前处理一旦开启即按该比例补边，设为 0 表示不补。",
			Default:     0.015,
			Min:         ocrBound(0),
			Max:         ocrBound(0.2),
			Group:       OCRFieldGroupAdvanced,
		},
	}
}

var ocrPipelineSpecs = []OCRPipelineSpec{
	{
		ID:          OCRPipelineManual,
		Name:        "手动",
		Description: "按固定参数执行 OCR 流水线，所有页面走同一条处理路径，结果可预期、便于复现。",
		Note:        OCROnlyNewDocumentsNote,
		Fields:      manualOCRPipelineFields,
		Rules:       []OCRPipelineRules{preprocessRule()},
	},
	{
		ID:          OCRPipelineSmart,
		Name:        "智能",
		Description: "先做页面分类（文本页/扫描页/图片页/混合页），再按页类型选择处理路径：文本页走文本层，图片页与扫描页渲染送 OCR/VLM，混合页两者兼取；扫描密集文档自动提高渲染 DPI。",
		Note:        OCROnlyNewDocumentsNote,
		Fields:      smartOCRPipelineFields,
		Rules:       []OCRPipelineRules{preprocessRule()},
	},
}

// OCRPipelineSpecs 返回全部已注册流水线的自描述清单，顺序稳定（manual 在前），
// 供配置面板一次性拉取：字段清单、默认值、取值范围与「仅对新解析文档生效」
// 的提示文案都在里面，前端不需要硬编码任何一条。
func OCRPipelineSpecs() []OCRPipelineSpec {
	out := make([]OCRPipelineSpec, 0, len(ocrPipelineSpecs))
	for _, spec := range ocrPipelineSpecs {
		fields := make([]OCRFieldDef, len(spec.Fields))
		copy(fields, spec.Fields)
		rules := make([]OCRPipelineRules, len(spec.Rules))
		copy(rules, spec.Rules)
		spec.Fields = fields
		spec.Rules = rules
		out = append(out, spec)
	}
	return out
}

// OCRPipelineSpecFor 按 id 取一条流水线的自描述；未知 id 回落手动流水线。
func OCRPipelineSpecFor(pipelineID OCRPipelineID) OCRPipelineSpec {
	id := NormalizeOCRPipelineID(pipelineID)
	for _, spec := range OCRPipelineSpecs() {
		if spec.ID == id {
			return spec
		}
	}
	return OCRPipelineSpecs()[0]
}

// OCRPipelineFields 返回指定流水线声明的参数清单；未知 id 回落手动流水线。
func OCRPipelineFields(pipelineID OCRPipelineID) []OCRFieldDef {
	return OCRPipelineSpecFor(pipelineID).Fields
}

// OCRPipelineRulesFor 返回指定流水线声明的规则清单。
func OCRPipelineRulesFor(pipelineID OCRPipelineID) []OCRPipelineRules {
	return OCRPipelineSpecFor(pipelineID).Rules
}

// OCRFieldLookup 在参数清单里按裸键找声明。
func OCRFieldLookup(fields []OCRFieldDef, key string) (OCRFieldDef, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f, true
		}
	}
	return OCRFieldDef{}, false
}

// --- 取值校验：越界拒绝并回落默认，不做 clamp ---------------------------------

// coerceOCRPipelineValue 把一个存储值（可能来自 JSON，因此数字统一是
// float64，也可能是字符串）按字段声明的类型转换。转换失败返回 ok=false。
func coerceOCRPipelineValue(field OCRFieldDef, raw any) (any, bool) {
	switch field.Type {
	case OCRFieldTypeBool:
		switch v := raw.(type) {
		case bool:
			return v, true
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "1", "true", "yes", "y", "on":
				return true, true
			case "0", "false", "no", "n", "off", "":
				return false, true
			}
		case float64:
			return v != 0, true
		case int:
			return v != 0, true
		}
		return nil, false
	case OCRFieldTypeInt:
		f, ok := toOCRPipelineFloat(raw)
		if !ok {
			return nil, false
		}
		return int(f), true
	case OCRFieldTypeFloat:
		f, ok := toOCRPipelineFloat(raw)
		if !ok {
			return nil, false
		}
		return f, true
	case OCRFieldTypeEnum:
		s, ok := raw.(string)
		if !ok {
			return nil, false
		}
		s = strings.TrimSpace(s)
		for _, opt := range field.Options {
			if strings.EqualFold(opt, s) {
				return strings.ToLower(opt), true
			}
		}
		return nil, false
	default:
		// note 等不承载取值的字段直接接受。
		return raw, true
	}
}

func toOCRPipelineFloat(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

func withinOCRPipelineRange(field OCRFieldDef, v float64) bool {
	if field.Min != nil && v < *field.Min {
		return false
	}
	if field.Max != nil && v > *field.Max {
		return false
	}
	return true
}

// ValidateOCRPipelineFieldValue 校验单个取值：类型正确且在声明范围内。
// 返回值是规范化后的取值（int 字段返回 int，enum 返回小写选项）。
func ValidateOCRPipelineFieldValue(field OCRFieldDef, raw any) (any, error) {
	if field.Type == OCRFieldTypeNote {
		return nil, nil
	}
	v, ok := coerceOCRPipelineValue(field, raw)
	if !ok {
		return nil, &OCRPipelineValidationError{
			MessageKey: "ocr.param.invalid_type",
			Field:      field.Key,
			Message:    "参数 " + field.Key + " 取值类型或内容非法，期望 " + string(field.Type),
		}
	}
	switch n := v.(type) {
	case int:
		if !withinOCRPipelineRange(field, float64(n)) {
			return nil, outOfRangeError(field)
		}
	case float64:
		if !withinOCRPipelineRange(field, n) {
			return nil, outOfRangeError(field)
		}
	}
	return v, nil
}

func outOfRangeError(field OCRFieldDef) error {
	msg := "参数 " + field.Key + " 超出取值范围"
	switch {
	case field.Min != nil && field.Max != nil:
		msg += " [" + formatOCRBound(*field.Min) + ", " + formatOCRBound(*field.Max) + "]"
	case field.Min != nil:
		msg += "（最小 " + formatOCRBound(*field.Min) + "）"
	case field.Max != nil:
		msg += "（最大 " + formatOCRBound(*field.Max) + "）"
	}
	if len(field.Options) > 0 {
		msg += "，可选值：" + strings.Join(field.Options, "/")
	}
	return &OCRPipelineValidationError{
		MessageKey: "ocr.param.out_of_range",
		Field:      field.Key,
		Message:    msg,
	}
}

func formatOCRBound(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// SanitizeOCRPipelineParams 逐项校验一张参数表，返回「通过校验的规范化取值」
// 与「被拒绝的键」。被拒绝的键不会出现在结果里 —— 运行时读到缺失就回落到
// 字段声明的默认值，这正是「越界拒绝并回落默认，不 clamp」的落地方式。
// 未声明的键原样保留（向前兼容：新版本存下的配置在旧版本上仍可读）。
func SanitizeOCRPipelineParams(fields []OCRFieldDef, params map[string]any, pipelineID string) (map[string]any, []string) {
	out := make(map[string]any, len(params))
	rejected := make([]string, 0)
	for key, raw := range params {
		bare := key
		if pipelineID != "" {
			bare = strings.TrimPrefix(key, pipelineID+".")
		}
		field, ok := OCRFieldLookup(fields, bare)
		if !ok {
			out[key] = raw
			continue
		}
		if field.Type == OCRFieldTypeNote {
			continue
		}
		v, err := ValidateOCRPipelineFieldValue(field, raw)
		if err != nil {
			rejected = append(rejected, key)
			continue
		}
		out[SlotKey(pipelineID, bare)] = v
	}
	return out, rejected
}

// DefaultOCRPipelineParams 返回一条流水线全部参数的默认值（键为 slot 键）。
// 配置面板可以用它把一份旧配置补全成完整的一份。
func DefaultOCRPipelineParams(pipelineID OCRPipelineID) map[string]any {
	id := NormalizeOCRPipelineID(pipelineID).String()
	fields := OCRPipelineFields(OCRPipelineID(id))
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if f.Type == OCRFieldTypeNote {
			continue
		}
		out[SlotKey(id, f.Key)] = f.Default
	}
	return out
}

// DefaultPreprocessParams 返回前处理参数的默认值（默认全部关闭）。
func DefaultPreprocessParams() map[string]any {
	fields := PreprocessFields()
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		out[f.Key] = f.Default
	}
	return out
}
