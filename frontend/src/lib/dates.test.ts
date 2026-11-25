import { afterAll, beforeAll, describe, expect, it } from 'vitest'

import { formatRun } from '../apps/cron/when'
import { formatModTime } from '../apps/files/listing'
import { formatClock } from './format'
import { loadLang, t } from './i18n'

describe('dates in Korean', () => {
  beforeAll(() => loadLang('ko'))
  afterAll(() => loadLang('vi'))

  it('writes the menu bar clock as month, day and weekday', () => {
    expect(formatClock(new Date(2026, 0, 5, 9, 4), t.menubar)).toBe('1월 5일 (월)  09:04')
  })

  it('writes a file date as year, month and day', () => {
    const modified = new Date(2026, 0, 3, 12, 0).getTime() / 1000
    expect(formatModTime(modified, new Date(2026, 9, 27), t.files)).toBe('2026. 1. 3.')
  })

  it('writes a cron run as month, day and weekday', () => {
    const now = Date.UTC(2026, 9, 27, 7, 7)
    expect(formatRun(Date.UTC(2026, 9, 29, 2, 30) / 1000, 'UTC', now)).toBe('10월 29일 (목) 02:30')
  })
})
