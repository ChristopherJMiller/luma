package protocol

// MaxAttachmentBytes bounds one attachment (images, PDF and text). The user
// copy says "20 MB"; the limit is 20 MiB (S18). The TS mirror is
// MAX_ATTACHMENT_BYTES.
const MaxAttachmentBytes = 20 << 20

// MaxTreeAttachmentBytes bounds all attachments of one conversation tree.
const MaxTreeAttachmentBytes = 20 << 20

// MaxImageMegapixels is the decoded size above which an image is refused.
const MaxImageMegapixels = 40

// MaxImageLongEdge is the long edge, in pixels, images are downscaled to.
const MaxImageLongEdge = 1568

// MaxToolURLLength bounds a WebFetch URL carried in a tool summary.
const MaxToolURLLength = 2048

// MaxProfileBytes bounds profile.md (about 1.5k tokens).
const MaxProfileBytes = 6000
