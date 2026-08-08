// Package secrets seals and unseals credentials with AES-256-GCM against a
// versioned keyring (SPEC §D.8).
//
// It is the ONE keyring in the process. `channel_credentials` is the generic
// sealed-secret store, and `alert_sources.auth_credential_id` reuses it, so a
// second implementation would mean a second key-rotation story for rows in one
// table.
//
// The envelope is: 12-byte random nonce ‖ ciphertext ‖ 16-byte GCM tag, with the
// credential's `kind` bound in as additional authenticated data so a blob cannot
// be moved from one kind to another, and a `key_version` recorded beside the row
// so a rotation can decrypt what earlier generations sealed.
//
// This package declares no ports. Its consumers declare theirs
// (`channels/repository.Sealer`, `channels/repository.Unsealer`,
// `sources/repository.Unsealer`, `notification/service.CredentialUnsealer`) and
// *Keyring satisfies all four by method set; `internal/app` is the only place
// they meet. That is deliberate: depguard forbids `internal/platform/**` from
// importing a domain.
package secrets
