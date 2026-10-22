/** The C3 error codes: wire values in REST errors, `error` events and link frames. */
export type ErrorCode =
  | 'rate_limited'
  | 'overloaded'
  | 'conductor_restarted'
  | 'budget_exhausted'
  | 'budget_warning'
  | 'maintenance'
  | 'capacity'
  | 'busy'
  | 'attachment_too_large'
  | 'image_too_large'
  | 'pdf_too_long'
  | 'conversation_attachments_full'
  | 'unsupported_attachment'
  | 'memory_full'
  | 'memory_disabled'
  | 'memory_pending_limit'
  | 'forget_too_short'
  | 'not_found'
  | 'invalid_request'
  | 'request_too_large'
  | 'upstream_error'
  | 'not_invited'

/**
 * The C3 user-facing copy, with its {placeholders} unexpanded. `satisfies` sits
 * on the declaration, so `make lint`'s typecheck is the exhaustiveness check:
 * a code without copy, or copy without a code, does not compile (S20).
 */
export const ERROR_COPY = {
  rate_limited: 'Luma is getting a lot of requests right now. Wait a moment and try again.',
  overloaded: 'Claude is overloaded right now. Try again in a minute.',
  conductor_restarted: 'Luma restarted while answering. Your conversation is safe — send your message again.',
  budget_exhausted: "Luma has used this month's budget. It will be back on {resets_at, local date}.",
  budget_warning: "Luma has used {pct}% of this month's budget.",
  maintenance: 'Luma is down for maintenance. Try again later.',
  capacity: 'Luma is busy with other conversations. Try again in a minute.',
  busy: 'Luma is still answering in this conversation.',
  attachment_too_large: 'That file is over 20 MB.',
  image_too_large: 'That image is too large. Photos up to 40 megapixels work.',
  pdf_too_long: 'That PDF has more than {max} pages, the most Luma reads in this mode.',
  conversation_attachments_full: 'This conversation already holds 20 MB of files. Start a new chat to add more.',
  unsupported_attachment: 'Luma can read photos, PDFs and text files.',
  memory_full: "The profile is full. Ask the person to tidy it in Luma's profile page.",
  memory_disabled: 'Memory is off in this conversation.',
  memory_pending_limit: "Several memory changes are already waiting for the person's review. Don't save more in this turn.",
  forget_too_short: 'Say at least 3 characters of what to forget.',
  not_found: "Luma can't find that. It may have been deleted.",
  invalid_request: "Luma didn't understand that request. Reload the page and try again.",
  request_too_large: 'That message is too long for Luma. Shorten it and try again.',
  upstream_error: 'Something went wrong talking to Claude. Try again.',
  not_invited: "This account isn't invited to Luma.",
} satisfies Record<ErrorCode, string>

/** The body of a REST error and the error of an rpc.result. */
export interface ErrorInfo {
  code: ErrorCode
  message: string
}
