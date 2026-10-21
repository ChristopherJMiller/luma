/**
 * One attachment's bound (images, PDF and text). The user copy says "20 MB";
 * the limit is 20 MiB (S18). The Go mirror is protocol.MaxAttachmentBytes.
 */
export const MAX_ATTACHMENT_BYTES = 20 * 1024 * 1024

/** All attachments of one conversation tree. */
export const MAX_TREE_ATTACHMENT_BYTES = 20 * 1024 * 1024

/** Decoded size above which an image is refused. */
export const MAX_IMAGE_MEGAPIXELS = 40

/** The long edge, in pixels, images are downscaled to. */
export const MAX_IMAGE_LONG_EDGE = 1568

/** A WebFetch URL carried in a tool summary. */
export const MAX_TOOL_URL_LENGTH = 2048

/** profile.md (about 1.5k tokens). */
export const MAX_PROFILE_BYTES = 6000
