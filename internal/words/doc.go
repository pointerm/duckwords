// Package words loads and normalizes a bounded word bank, extracts Unicode-letter
// tokens, applies validated glob filters, and accumulates post-local counts.
//
// Normalization is deliberately locale-independent lowercase without NFC or NFKC.
// Consequently, precomposed letters remain letters while combining marks and invalid
// UTF-8 delimit tokens and invalidate dictionary or filter entries.
package words
