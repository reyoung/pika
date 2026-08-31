import { beforeEach, describe, expect, it } from 'vitest'
import { importFragmentToken, tokenStorageKey } from './api'

describe('fragment token import', () => {
  beforeEach(() => sessionStorage.clear())
  it('moves the token into sessionStorage and immediately clears the fragment', () => {
    history.replaceState(null, '', '/#token=one-time-secret')
    expect(importFragmentToken()).toBe('one-time-secret')
    expect(sessionStorage.getItem(tokenStorageKey)).toBe('one-time-secret')
    expect(location.hash).toBe('')
  })
})
