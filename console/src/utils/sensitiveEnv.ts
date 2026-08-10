/**
 * Mirrors the heuristics in `kip/cmd/env.go` so the console and the CLI flag the
 * same variables. Environment variables are stored in the App CR's spec.env and
 * are copied verbatim by `kip export`, so a credential set here is far more
 * exposed than one in the Secrets tab.
 */

import { stripPlaceholders } from './envTemplate'

const SECRET_NAME_TOKENS = [
  'PASSWORD',
  'PASSWD',
  'SECRET',
  'TOKEN',
  'APIKEY',
  'API_KEY',
  'ACCESS_KEY',
  'PRIVATE_KEY',
  'CREDENTIAL',
  'PASSPHRASE',
]

/** Matches a connection string carrying a literal password, as in postgresql://kipper:s3cret@host:5432/app. */
const EMBEDDED_USERINFO = /:\/\/[^/@\s]*:[^/@\s]+@/

/**
 * Reports whether a variable name suggests a sensitive value.
 */
export function looksLikeSecretName(key: string): boolean {
  const upper = key.toUpperCase()
  return SECRET_NAME_TOKENS.some(token => upper.includes(token))
}

/**
 * Reports whether a value carries a credential whatever its key is called.
 * `looksLikeSecretName` matches none of DATABASE_URL, so a URL with an embedded
 * password would otherwise be stored in silence.
 *
 * Placeholders are removed before the test, because a templated URL resolves its
 * credential into a Secret and never stores one on the CR. Flagging it would warn
 * against the safe construction and teach people to ignore the warning.
 *
 * The stripping goes through the parser in `envTemplate.ts`, so the warning and
 * the resolver read one grammar. It used to be a regex here, which could not
 * express the `$${NAME}` escape: an escaped placeholder is literal text and its
 * `:` and `@` belong to the value, so stripping it hid a delimiter that is
 * really there.
 */
export function valueLooksLikeCredential(value: string): boolean {
  return EMBEDDED_USERINFO.test(stripPlaceholders(value))
}

/**
 * Reports whether an environment variable should carry a plain-text warning,
 * by name or by value.
 */
export function isSensitiveEnvVar(key: string, value: string): boolean {
  return looksLikeSecretName(key) || valueLooksLikeCredential(value)
}
