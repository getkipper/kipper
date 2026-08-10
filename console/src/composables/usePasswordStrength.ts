import { computed, type Ref } from 'vue'

interface PasswordCheck {
  label: string
  met: boolean
}

interface PasswordStrength {
  checks: PasswordCheck[]
  score: number      // 0-100
  allMet: boolean
  color: string      // CSS gradient color stop
  label: string      // "Weak", "Fair", "Good", "Strong"
}

export function usePasswordStrength(password: Ref<string>) {
  const strength = computed<PasswordStrength>(() => {
    const pw = password.value

    const checks: PasswordCheck[] = [
      { label: 'At least 8 characters', met: pw.length >= 8 },
      { label: 'Lowercase letter', met: /[a-z]/.test(pw) },
      { label: 'Uppercase letter', met: /[A-Z]/.test(pw) },
      { label: 'Number', met: /\d/.test(pw) },
      { label: 'Symbol', met: /[^a-zA-Z0-9]/.test(pw) },
    ]

    const metCount = checks.filter(c => c.met).length
    const allMet = metCount === checks.length

    // Score: base from checks met, bonus for length
    let score = (metCount / checks.length) * 80
    if (pw.length >= 12) score += 10
    if (pw.length >= 16) score += 10
    score = Math.min(100, Math.round(score))

    let label = 'Weak'
    let color = '#ef4444' // red
    if (score >= 80) {
      label = 'Strong'
      color = '#22c55e' // green
    } else if (score >= 60) {
      label = 'Good'
      color = '#84cc16' // lime
    } else if (score >= 40) {
      label = 'Fair'
      color = '#eab308' // yellow
    }

    return { checks, score, allMet, color, label }
  })

  return { strength }
}
