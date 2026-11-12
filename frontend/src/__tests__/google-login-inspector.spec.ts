import { describe, expect, it } from 'vitest'

import source from '../../../backend/internal/pkg/camoufox/google_login.js?raw'
function inspect(html:string,url='https://accounts.google.com/signin',top=true){
  document.body.innerHTML=html
  Object.defineProperty(HTMLElement.prototype,'innerText',{configurable:true,get(this: HTMLElement){return this.textContent}})
  HTMLElement.prototype.getClientRects=function(this: HTMLElement){return [{width:200,height:Number(this.getAttribute('data-height') || '80')} as DOMRect] as unknown as DOMRectList}
  HTMLElement.prototype.getBoundingClientRect=function(this: HTMLElement){return {left:0,top:0,width:200,height:Number(this.getAttribute('data-height') || '80')} as DOMRect}
  const fn=window.eval('('+source+')')
  const result=JSON.parse(fn('owner@example.test',document,new URL(url),top));document.body.innerHTML='';return result
}
describe('Google login page classification',()=>{
  it('recognizes email, password and authenticator fields',()=>{expect(inspect('<input id="identifierId"><button id="identifierNext">Next</button>').phase).toBe('email');expect(inspect('<input type="password" id="password" value="private-marker">').phase).toBe('password');expect(inspect('<input name="totpPin">').phase).toBe('totp');expect(JSON.stringify(inspect('<input type="password" id="pwd" value="private-marker">'))).not.toContain('private-marker')})
  it('distinguishes a CAPTCHA challenge from a small badge',()=>{expect(inspect('<input id="identifierId"><iframe src="https://www.google.com/recaptcha/api2/anchor" data-height="60"></iframe>').phase).toBe('email');expect(inspect('<iframe src="https://www.google.com/recaptcha/api2/bframe" data-height="200"></iframe>').reason).toBe('captcha')})
  it('routes unknown security challenges and rejected passwords to the correct state',()=>{expect(inspect('<p>Verify it is you</p>','https://accounts.google.com/signin/challenge/ipp').reason).toBe('security_verification');expect(inspect('<p>Wrong password</p><input type="password">').reason).toBe('invalid_credentials')})
  it('does not trust another origin or an embedded login frame',()=>{expect(inspect('<input type="password">','https://example.test').reason).toBe('unexpected_page');expect(inspect('<input type="password">','https://accounts.google.com',false).reason).toBe('unexpected_page');expect(inspect('', 'about:blank').phase).toBe('waiting')})
  it('does not confuse the normal footer with onboarding',()=>{expect(inspect('<textarea aria-label="Enter a prompt"></textarea><footer>Terms of Service</footer>','https://aistudio.google.com/prompts/new_chat').phase).toBe('studio')})
  it('recognizes onboarding before declaring the studio ready',()=>{expect(inspect('<p>Terms of Service</p><label><input id="age" type="checkbox">I am 18 years old</label><button>Continue</button>','https://aistudio.google.com/welcome').phase).toBe('onboarding_check')})
  it('recognizes Material hosts, hidden native inputs and duplicate checkbox roles',()=>{const html='<h1>Welcome to Google AI Studio</h1><p>Gemini API Additional Terms of Service</p><mat-checkbox role="checkbox" aria-label="I acknowledge that I am at least 18 years old, and a developer"><input type="checkbox" style="visibility:hidden"><span class="mdc-checkbox__background"></span></mat-checkbox><mat-checkbox role="checkbox" aria-label="Opt in to receive news, offers, promotions, and updates about Google AI."><input type="checkbox"><span class="mdc-checkbox__background"></span></mat-checkbox><button disabled>Continue <mat-icon>arrow_forward</mat-icon></button>';const result=inspect(html,'https://aistudio.google.com/onboarding');expect(result.phase).toBe('onboarding_check');expect(result.key).toContain('18 years');expect(result.key).not.toContain('Opt in')})
  it('continues after the required checkbox while leaving marketing unchecked',()=>{const html='<p>Additional Terms of Service</p><label><input type="checkbox" checked>I acknowledge I am at least 18 years old</label><label><input type="checkbox">I would like to receive emails for model updates and offers</label><button>Continue <mat-icon aria-hidden="true">arrow_forward</mat-icon></button>';expect(inspect(html,'https://aistudio.google.com/onboarding').phase).toBe('onboarding_continue')})
  it('uses aria-labelledby and scopes consent to the open terms dialog',()=>{const html='<label><input type="checkbox">Unrelated setting</label><div role="dialog"><p>Terms of Service</p><span id="terms-name">I agree to the terms</span><div role="checkbox" aria-labelledby="terms-name" aria-checked="false"></div><button>Continue</button></div>';expect(inspect(html,'https://aistudio.google.com/prompts/new_chat').phase).toBe('onboarding_check')})
  it('skips the optional intent step even when it uses the same onboarding URL',()=>{expect(inspect('<h1>What would you like to build?</h1><button>Skip <mat-icon>arrow_forward</mat-icon></button>','https://aistudio.google.com/onboarding').phase).toBe('optional_skip')})
  it('waits for unknown or disabled consent instead of clicking through',()=>{expect(inspect('<p>Terms of Service</p><label><input type="checkbox">An unfamiliar selection</label><button>Continue</button>','https://aistudio.google.com/onboarding').phase).toBe('onboarding_wait');expect(inspect('<p>Terms of Service</p><label><input type="checkbox" checked>I agree to the terms</label><button aria-disabled="true">Continue</button>','https://aistudio.google.com/onboarding').phase).toBe('onboarding_wait')})
  it('does not mistake an existing account prompt mentioning age and terms for onboarding',()=>{expect(inspect('<textarea>Explain the age requirement for 18 years old.</textarea><footer>Terms of Service</footer>','https://aistudio.google.com/prompts/new_chat').phase).toBe('studio')})

  it('declines the optional selfie-recovery enrollment on trusted Google pages',()=>{
    const html='<h1>Protect your account<br>access with a selfie</h1><p>Saving a selfie video can help you get into your Google Account if you have trouble signing in.</p><button>Not now</button><button>Continue</button>'
    for(const origin of ['https://accounts.google.com','https://myaccount.google.com','https://gds.google.com']) {
      for(const decline of ['Not now','Skip','Skip for now','跳过','跳過','暂时跳过']) {
        const result=inspect(html.replace('Not now',decline),origin+'/v3/signin/challenge/recovery');expect(result.phase).toBe('optional_skip');expect(result.key).toBe('optional-selfie-skip')
      }
    }
    expect(inspect(html,'https://accounts.google.com.evil.test/').reason).toBe('unexpected_page')
  })
  it('keeps mandatory or unrecognized identity verification manual',()=>{
    expect(inspect('<h1>Protect your account access with a selfie</h1><button>Continue</button>','https://accounts.google.com/signin/challenge/selfie').phase).toBe('manual')
    expect(inspect('<h1>Verify it is you</h1><button>Not now</button>','https://accounts.google.com/signin/challenge/ipp').phase).toBe('manual')
    expect(inspect('<input type="password"><button>Not now</button>','https://myaccount.google.com/').phase).toBe('manual')
    expect(inspect('<h1>Protect your account access with a selfie</h1>','https://accounts.google.com/signin/challenge/recovery').phase).toBe('waiting')
  })

})
