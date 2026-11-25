import { describe, expect, it } from 'vitest'

import { displayAddress, frameURL, parseAddress, serverAddress } from './address'

describe('parseAddress', () => {
  it('reads the forms people type', () => {
    expect(parseAddress('localhost:3000/admin?x=1')).toEqual({
      port: 3000,
      path: '/admin?x=1',
      secure: false,
    })
    expect(parseAddress(':8080')).toEqual({ port: 8080, path: '/', secure: false })
    expect(parseAddress('3000/api')).toEqual({ port: 3000, path: '/api', secure: false })
    expect(parseAddress('http://127.0.0.1:5173/#/home')).toEqual({
      port: 5173,
      path: '/#/home',
      secure: false,
    })
    expect(parseAddress('localhost')).toEqual({ port: 80, path: '/', secure: false })
  })

  it('reads HTTPS addresses, on 443 unless a port is given', () => {
    expect(parseAddress('https://localhost:8443/a')).toEqual({
      port: 8443,
      path: '/a',
      secure: true,
    })
    expect(parseAddress('https://127.0.0.1')).toEqual({ port: 443, path: '/', secure: true })
    // 443 without a scheme is still HTTP: the scheme is never guessed.
    expect(parseAddress('localhost:443')).toEqual({ port: 443, path: '/', secure: false })
  })

  it('reads back what the bar shows', () => {
    const shown = ['localhost/', 'localhost:443/', 'https://localhost/', 'https://localhost:80/x']
    for (const text of shown) expect(serverAddress(parseAddress(text)!)).toBe(text)
  })

  it('refuses what is not the server', () => {
    expect(parseAddress('github.com')).toBeUndefined()
    expect(parseAddress('https://github.com')).toBeUndefined()
    expect(parseAddress('ftp://localhost:21')).toBeUndefined()
    expect(parseAddress('localhost:99999')).toBeUndefined()
    expect(parseAddress('')).toBeUndefined()
  })
})

describe('frame and display addresses', () => {
  it('adds the token for the frame and hides it in the bar', () => {
    const url = frameURL(51000, '/admin?x=1#top', 'tok')
    expect(url).toBe('http://127.0.0.1:51000/admin?x=1&__davish=tok#top')
    const http = (remotePort: number) => ({ remotePort, secure: false })
    const https = (remotePort: number) => ({ remotePort, secure: true })
    expect(displayAddress(url, http(3000))).toBe('localhost:3000/admin?x=1#top')
    expect(displayAddress('http://127.0.0.1:51000/', http(80))).toBe('localhost/')
    expect(displayAddress(url, https(8443))).toBe('https://localhost:8443/admin?x=1#top')
    expect(displayAddress('http://127.0.0.1:51000/', https(443))).toBe('https://localhost/')
  })
})
