package protocol

// ErrorCode is a C3 code. Codes are wire values: they appear in REST error
// bodies, `error` events and link frames.
type ErrorCode string

const (
	CodeRateLimited                 ErrorCode = "rate_limited"
	CodeOverloaded                  ErrorCode = "overloaded"
	CodeConductorRestarted          ErrorCode = "conductor_restarted"
	CodeBudgetExhausted             ErrorCode = "budget_exhausted"
	CodeBudgetWarning               ErrorCode = "budget_warning"
	CodeMaintenance                 ErrorCode = "maintenance"
	CodeCapacity                    ErrorCode = "capacity"
	CodeBusy                        ErrorCode = "busy"
	CodeAttachmentTooLarge          ErrorCode = "attachment_too_large"
	CodeImageTooLarge               ErrorCode = "image_too_large"
	CodePDFTooLong                  ErrorCode = "pdf_too_long"
	CodeConversationAttachmentsFull ErrorCode = "conversation_attachments_full"
	CodeUnsupportedAttachment       ErrorCode = "unsupported_attachment"
	CodeMemoryFull                  ErrorCode = "memory_full"
	CodeMemoryDisabled              ErrorCode = "memory_disabled"
	CodeMemoryPendingLimit          ErrorCode = "memory_pending_limit"
	CodeForgetTooShort              ErrorCode = "forget_too_short"
	CodeNotFound                    ErrorCode = "not_found"
	CodeInvalidRequest              ErrorCode = "invalid_request"
	CodeRequestTooLarge             ErrorCode = "request_too_large"
	CodeUpstreamError               ErrorCode = "upstream_error"
	CodeNotInvited                  ErrorCode = "not_invited"
)

// ErrorCopy is the C3 user-facing copy, with its {placeholders} unexpanded.
var ErrorCopy = map[ErrorCode]string{
	CodeRateLimited:                 "Luma is getting a lot of requests right now. Wait a moment and try again.",
	CodeOverloaded:                  "Claude is overloaded right now. Try again in a minute.",
	CodeConductorRestarted:          "Luma restarted while answering. Your conversation is safe — send your message again.",
	CodeBudgetExhausted:             "Luma has used this month's budget. It will be back on {resets_at, local date}.",
	CodeBudgetWarning:               "Luma has used {pct}% of this month's budget.",
	CodeMaintenance:                 "Luma is down for maintenance. Try again later.",
	CodeCapacity:                    "Luma is busy with other conversations. Try again in a minute.",
	CodeBusy:                        "Luma is still answering in this conversation.",
	CodeAttachmentTooLarge:          "That file is over 20 MB.",
	CodeImageTooLarge:               "That image is too large. Photos up to 40 megapixels work.",
	CodePDFTooLong:                  "That PDF has more than {max} pages, the most Luma reads in this mode.",
	CodeConversationAttachmentsFull: "This conversation already holds 20 MB of files. Start a new chat to add more.",
	CodeUnsupportedAttachment:       "Luma can read photos, PDFs and text files.",
	CodeMemoryFull:                  "The profile is full. Ask the person to tidy it in Luma's profile page.",
	CodeMemoryDisabled:              "Memory is off in this conversation.",
	CodeMemoryPendingLimit:          "Several memory changes are already waiting for the person's review. Don't save more in this turn.",
	CodeForgetTooShort:              "Say at least 3 characters of what to forget.",
	CodeNotFound:                    "Luma can't find that. It may have been deleted.",
	CodeInvalidRequest:              "Luma didn't understand that request. Reload the page and try again.",
	CodeRequestTooLarge:             "That message is too long for Luma. Shorten it and try again.",
	CodeUpstreamError:               "Something went wrong talking to Claude. Try again.",
	CodeNotInvited:                  "This account isn't invited to Luma.",
}

// ErrorInfo is the body of a REST error and the error of an rpc.result.
type ErrorInfo struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}
