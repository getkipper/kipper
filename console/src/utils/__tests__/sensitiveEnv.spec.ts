import { describe, it, expect } from 'vitest'

import { isSensitiveEnvVar, looksLikeSecretName, valueLooksLikeCredential } from '../sensitiveEnv'

describe('looksLikeSecretName', () => {
  it('flags names that suggest a credential', () => {
    for (const key of ['DB_PASSWORD', 'API_KEY', 'STRIPE_SECRET', 'GITHUB_TOKEN', 'apikey']) {
      expect(looksLikeSecretName(key), key).toBe(true)
    }
  })

  it('leaves ordinary names alone', () => {
    for (const key of ['LOG_LEVEL', 'API_URL', 'REGION', 'PORT', 'PUBLIC_HOST']) {
      expect(looksLikeSecretName(key), key).toBe(false)
    }
  })
})

// The name heuristic alone missed the case that prompted this check: a real
// DocuSeal deployment stored its database password inside DATABASE_URL, whose
// name matches none of the tokens above, so nothing warned.
describe('valueLooksLikeCredential', () => {
  it('flags a literal password embedded in a connection string', () => {
    expect(valueLooksLikeCredential('postgresql://kipper:s3cr3t@db:5432/app')).toBe(true)
    expect(valueLooksLikeCredential('redis://:hunter2@cache:6379/0')).toBe(true)
  })

  it('leaves a URL without credentials alone', () => {
    expect(valueLooksLikeCredential('postgresql://db:5432/app')).toBe(false)
  })

  it('leaves a templated URL alone, because the credential never reaches the CR', () => {
    expect(
      valueLooksLikeCredential('postgresql://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}'),
    ).toBe(false)
    expect(valueLooksLikeCredential('postgresql://kipper:${DB_PASSWORD}@db:5432/app')).toBe(false)
    expect(valueLooksLikeCredential('jdbc:postgresql://${DB_HOST}:${DB_PORT}/orders')).toBe(false)
  })

  it('still flags a literal password beside a templated username', () => {
    expect(valueLooksLikeCredential('postgresql://${DB_USERNAME}:s3cr3t@db:5432/app')).toBe(true)
  })

  it('treats the urlencode modifier as part of a placeholder', () => {
    expect(
      valueLooksLikeCredential('postgresql://${DB_USERNAME:urlencode}:${DB_PASSWORD:urlencode}@db:5432/app'),
    ).toBe(false)
  })

  // A placeholder-shaped run that the resolver would not resolve must not erase
  // the URL's own delimiters. In the first case the password really is `pass}`.
  it('cannot be suppressed by a malformed placeholder', () => {
    expect(valueLooksLikeCredential('postgresql://user${:pass}@db:5432/app')).toBe(true)
    expect(valueLooksLikeCredential('postgresql://user:${1pass}@db:5432/app')).toBe(true)
    expect(valueLooksLikeCredential('postgresql://user:${PW:base64}@db:5432/app')).toBe(true)
    expect(valueLooksLikeCredential('postgresql://user:${}pw@db:5432/app')).toBe(true)
  })
})

describe('isSensitiveEnvVar', () => {
  it('flags by name or by value', () => {
    expect(isSensitiveEnvVar('API_KEY', 'abc123')).toBe(true)
    expect(isSensitiveEnvVar('DATABASE_URL', 'postgresql://kipper:s3cr3t@db:5432/app')).toBe(true)
    expect(isSensitiveEnvVar('LOG_LEVEL', 'debug')).toBe(false)
  })
})
