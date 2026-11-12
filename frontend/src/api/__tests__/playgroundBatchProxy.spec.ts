import { describe, expect, it, vi } from 'vitest'
vi.mock('@/i18n', () => ({ getLocale: () => 'zh-CN' }))
import { parseBatchProxyText } from '@/api/admin/playgroundBatch'

describe('batch sign-in proxy import',()=>{
  it('parses the four-field format and keeps colons in passwords',()=>{expect(parseBatchProxyText('198.51.100.1:8080:user:secret:with:colons')).toEqual([{protocol:'http',host:'198.51.100.1',port:8080,username:'user',password:'secret:with:colons'}])})
  it('supports encoded proxy URLs and SOCKS5h',()=>{expect(parseBatchProxyText('socks5h://user:p%40ss@[2001:db8::1]:1080')[0]).toEqual({protocol:'socks5h',host:'2001:db8::1',port:1080,username:'user',password:'p@ss'})})
  it('rejects malformed endpoints without echoing their credentials',()=>{for(const line of ['host:70000:user:private-marker','ftp://u:private-marker@host:21','host:abc:user:private-marker','http://u:private-marker@host:8080/path']){try{parseBatchProxyText(line);throw new Error('accepted')}catch(error){expect((error as Error).message).not.toContain('private-marker');expect((error as Error).message).toContain('格式')}}})
})
