package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// ---------------------------------------------------------------------------
// Captcha + verification-code endpoints (P0-4,
// docs/prd/auth-dual-channel-verification.md §5-§6).
//
//   GET  /auth/captcha                  → fresh challenge (slider / text)
//   POST /auth/captcha/verify           → one-time captcha_token
//   POST /auth/verification-code/send   → SMS/email code (needs the token)
//
// All three are unauthenticated (see middleware.noAuthAPI) and IP
// rate-limited where abuse is expensive (the send endpoint).
// ---------------------------------------------------------------------------

// GetCaptchaChallenge godoc
// @Summary      获取人机验证挑战
// @Description  返回滑块拼图或数字图形验证码；答案留在服务端，验证通过后签发一次性 captcha_token
// @Tags         认证
// @Produce      json
// @Success      200  {object}  types.CaptchaChallengeResponse
// @Failure      503  {object}  errors.AppError  "验证码服务未启用"
// @Router       /auth/captcha [get]
func (h *AuthHandler) GetCaptchaChallenge(c *gin.Context) {
	ctx := c.Request.Context()
	if h.captchaSvc == nil {
		appErr := errors.NewServiceUnavailableError("captcha service not configured")
		c.Error(appErr)
		return
	}
	resp, err := h.captchaSvc.CreateChallenge(ctx)
	if err != nil {
		logger.Errorf(ctx, "[captcha] create challenge failed: %v", err)
		appErr := errors.NewInternalServerError("failed to create captcha challenge")
		c.Error(appErr)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// VerifyCaptchaChallenge godoc
// @Summary      校验人机验证
// @Description  校验滑块位置或数字答案；通过后返回一次性 captcha_token（10 分钟有效，仅可使用一次）
// @Tags         认证
// @Accept       json
// @Produce      json
// @Param        request  body  types.CaptchaVerifyRequest  true  "验证答案"
// @Success      200      {object}  types.CaptchaVerifyResponse
// @Failure      400      {object}  errors.AppError  "参数错误"
// @Failure      503      {object}  errors.AppError  "验证码服务未启用"
// @Router       /auth/captcha/verify [post]
func (h *AuthHandler) VerifyCaptchaChallenge(c *gin.Context) {
	ctx := c.Request.Context()
	if h.captchaSvc == nil {
		appErr := errors.NewServiceUnavailableError("captcha service not configured")
		c.Error(appErr)
		return
	}
	var req types.CaptchaVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		appErr := errors.NewValidationError("Invalid captcha parameters").WithDetails(err.Error())
		c.Error(appErr)
		return
	}
	resp, err := h.captchaSvc.VerifyChallenge(ctx, &req)
	if err != nil {
		logger.Errorf(ctx, "[captcha] verify challenge %s failed: %v", req.CaptchaID, err)
		appErr := errors.NewInternalServerError("failed to verify captcha")
		c.Error(appErr)
		return
	}
	// Success=false is a normal wrong-answer outcome, not a server error:
	// return 200 so the frontend can render "retry" instead of an error page.
	c.JSON(http.StatusOK, resp)
}

// SendVerificationCode godoc
// @Summary      发送短信/邮箱验证码
// @Description  向手机号或邮箱发送注册验证码。每次发送必须携带有效的人机验证 captcha_token；同一目标 60 秒内仅可发送一次，每日上限受配置限制
// @Tags         认证
// @Accept       json
// @Produce      json
// @Param        request  body  types.VerificationCodeSendRequest  true  "发送请求"
// @Success      200      {object}  object{success=boolean}
// @Failure      400      {object}  errors.AppError  "参数或格式错误"
// @Failure      429      {object}  errors.AppError  "发送过于频繁 / 超出当日限额"
// @Failure      503      {object}  errors.AppError  "通道未配置"
// @Router       /auth/verification-code/send [post]
func (h *AuthHandler) SendVerificationCode(c *gin.Context) {
	ctx := c.Request.Context()
	if h.verificationCodeSvc == nil {
		appErr := errors.NewServiceUnavailableError("verification code service not configured")
		c.Error(appErr)
		return
	}
	var req types.VerificationCodeSendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		appErr := errors.NewValidationError("Invalid verification code request").WithDetails(err.Error())
		c.Error(appErr)
		return
	}

	if err := h.verificationCodeSvc.Send(ctx, &req); err != nil {
		ve, ok := service.AsVerificationError(err)
		if !ok {
			logger.Errorf(ctx, "[verification-code] send failed: %v", err)
			appErr := errors.NewInternalServerError("failed to send verification code")
			c.Error(appErr)
			return
		}
		// Machine-readable code rides in Details so the frontend can i18n
		// the message and drive its resend countdown.
		appErr := errors.NewBadRequestError(ve.Message).WithDetails(ve.Code)
		switch ve.Code {
		case service.VerificationErrResendTooFrequent,
			service.VerificationErrDailyLimitExceeded:
			appErr = errors.NewTooManyRequestsError(ve.Message).WithDetails(ve.Code)
		case service.VerificationErrChannelDisabled:
			appErr = errors.NewServiceUnavailableError(ve.Message).WithDetails(ve.Code)
		case service.VerificationErrCaptchaRequired:
			appErr = errors.NewValidationError(ve.Message).WithDetails(ve.Code)
		}
		// The target itself is echoed back in logs only — never in the
		// response body, to avoid acting as an oracle for enumerated targets.
		logger.Warnf(ctx, "[verification-code] send rejected (%s): %s", ve.Code, ve.Message)
		c.Error(appErr)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "verification code sent",
	})
}

// captchaPublicType is a nil-safe accessor for the configured challenge
// flavour exposed via /auth/config.
func (h *AuthHandler) captchaPublicType() string {
	if h.captchaSvc == nil {
		return ""
	}
	return strings.TrimSpace(h.captchaSvc.ChallengeType())
}
