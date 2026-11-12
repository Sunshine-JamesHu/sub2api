(function inspectGoogleLogin(email, root = document, page = location, topLevel = window === window.top) {
  const visible = el => !!el && !el.disabled && el.getAttribute('aria-disabled') !== 'true' && el.getClientRects().length > 0 && el.getBoundingClientRect().width > 0 && el.getBoundingClientRect().height > 0 &&
    getComputedStyle(el).visibility !== 'hidden' && getComputedStyle(el).display !== 'none';
  const text = (root.body?.innerText || '').slice(0, 30000);
  const low = text.toLowerCase();
  const base = {origin: page.origin, path: page.pathname, phase: 'waiting', reason: ''};
  const target = (phase, el, next, stableKey) => {
    const rect = el.getBoundingClientRect();
    const token = Math.random().toString(36).slice(2);
    window.__sub2LoginTarget = {element: el, next, token, href: page.href};
    const key = stableKey || el.id || el.getAttribute('data-identifier') || el.getAttribute('data-challengetype') || (el.getAttribute('aria-label') || el.innerText || el.parentElement?.innerText || '').trim().slice(0, 100);
    return JSON.stringify({...base, phase, token, key, x: Math.round(rect.left + rect.width / 2), y: Math.round(rect.top + rect.height / 2)});
  };
  const result = (phase, reason = '') => JSON.stringify({...base, phase, reason});
  if (!topLevel) return result('manual', 'unexpected_page');
  if (page.href === 'about:blank' || page.href === 'about:newtab') return result('waiting');
  if (/^about:(neterror|certerror)/.test(page.href)) return result('failed', 'network_error');
  const accountSettingsOrigin = ['https://myaccount.google.com', 'https://gds.google.com'].includes(page.origin);
  if (!['https://accounts.google.com', 'https://aistudio.google.com', 'https://gemini.google.com'].includes(page.origin) && !accountSettingsOrigin) {
    return result('manual', 'unexpected_page');
  }
  const captcha = /\/recaptcha/.test(page.pathname) || /verify you are human|unusual traffic|i.m not a robot|我不是机器人|人机验证|異常流量/.test(low) ||
    [...root.querySelectorAll('iframe[src*="recaptcha"]')].some(el => {
      const r = el.getBoundingClientRect();
      return visible(el) && r.width >= 100 && r.height >= 70;
    });
  if (captcha) return result('manual', 'captcha');
  if (/wrong password|incorrect password|密码有误|密码错误|密碼錯誤|couldn.t find your google account|找不到您的.*账号/.test(low)) return result('failed', 'invalid_credentials');
  if (/wrong code|incorrect code|code you entered isn.t valid|验证码错误|驗證碼錯誤|验证码不正确/.test(low)) return result('manual', 'totp_rejected');
  if (/account has been disabled|account is disabled|帳戶已停用|帐号已停用|账号已停用/.test(low)) return result('failed', 'account_disabled');
  const buttons = [...root.querySelectorAll('button,[role="button"],a')].filter(visible);
  const label = el => {
    const aria = el.getAttribute('aria-label');
    if (aria?.trim()) return aria.trim();
    const ids = (el.getAttribute('aria-labelledby') || '').split(/\s+/).filter(Boolean);
    const referenced = ids.map(id => root.getElementById?.(id)?.textContent || '').join(' ').trim();
    if (referenced) return referenced.replace(/\s+/g, ' ');
    const copy = el.cloneNode(true);
    copy.querySelectorAll('[aria-hidden="true"],mat-icon,.material-icons,.material-symbols-outlined,script,style').forEach(x => x.remove());
    return (copy.textContent || '').replace(/\s+/g, ' ').trim();
  };
  const named = regex => buttons.find(el => regex.test(label(el)));
  // This is an optional recovery-method enrollment, not a request to supply a
  // selfie. Only decline a recognized prompt with an explicit Not now button.
  const optionalSelfie = /protect your account access with a selfie|saving a selfie video can help you get into your google account|使用自拍视频保护您的(?:帐号|账号)|使用自拍影片保護您的帳戶/i.test(low.replace(/\s+/g, ' '));
  if ((page.origin === 'https://accounts.google.com' || accountSettingsOrigin) && optionalSelfie) {
    const decline = named(/^(not now|skip|skip for now|no thanks|暂不|暂时不要|暂时跳过|跳过|跳過|略過|以后再说|稍后再说|暫不|稍後再說|あとで|今はしない|スキップ)$/i);
    if (decline) return target('optional_skip', decline, undefined, 'optional-selfie-skip');
    if (named(/^(continue|继续|繼續|続行)$/i)) return result('manual', 'security_verification');
    return result('waiting');
  }
  if (accountSettingsOrigin) return result('manual', 'security_verification');
  if (page.origin === 'https://accounts.google.com') {
    for (const [phase, selector, next] of [
      ['email', '#identifierId:not([type="hidden"])', '#identifierNext'],
      ['password', 'input[type="password"]', '#passwordNext'],
      ['totp', 'input[name="totpPin"],input#totpPin', '#totpNext'],
    ]) {
      const field = [...root.querySelectorAll(selector)].find(visible);
      if (field) return target(phase, field, next);
    }
    const own = [...root.querySelectorAll('[data-identifier]')].find(el => visible(el) && (el.getAttribute('data-identifier') || '').toLowerCase() === email.toLowerCase());
    if (own) return target('choose_account', own);
    const another = named(/^(use another account|使用其他帐号|使用其他账号|使用其他帳戶|使用另一个帐号)$/i);
    if (another) return target('other_account', another);
    const totp = [...root.querySelectorAll('[data-challengetype="6"]'), ...buttons].find(el => visible(el) &&
      (el.getAttribute('data-challengetype') === '6' || /authenticator|身份验证器|身分驗證器|身份验证应用|驗證器/.test(label(el).toLowerCase())));
    if (totp) return target('choose_totp', totp);
    if (/\/speedbump\/|\/uplevelingstep\//.test(page.pathname) || /simplify your sign-in/.test(low)) {
      const skip = named(/^(not now|skip|skip for now|no thanks|暂不|以后再说|略過|跳过|跳過|暂时跳过)$/i);
      if (skip) return target('optional_skip', skip);
    }
    if (/\/challenge\//.test(page.pathname)) return result('manual', 'security_verification');
    if (/couldn.t sign you in|this browser or app may not be secure|无法登录|無法登入/.test(low)) return result('manual', 'security_verification');
    return result('waiting');
  }
  const onboardingPath = /\/(welcome|onboarding|terms)(\/|$)/.test(page.pathname);
  // The optional intent questionnaire can use the same URL as the terms page.
  const skip = named(/^(skip|skip for now|跳过|跳過|暂时跳过|略過|スキップ)$/i);
  if (skip && (onboardingPath || /what do you (plan|want)|how will you use|what will you use|用途|使用目的/i.test(text))) {
    return target('optional_skip', skip);
  }
  const termsPattern = /terms of service|additional terms|服务条款|使用条款|服務條款|利用規約/i;
  const dialog = [...root.querySelectorAll('[role="dialog"],mat-dialog-container,dialog[open]')].find(el => visible(el) && termsPattern.test(el.innerText || el.textContent || ''));
  const welcome = [...root.querySelectorAll('h1,h2,[role="heading"]')].some(el => visible(el) && /welcome to google ai studio|欢迎.*ai studio|歡迎.*ai studio/i.test(label(el)));
  if (dialog || onboardingPath || (welcome && termsPattern.test(text))) {
    const scope = dialog || root;
    // Material widgets can expose both a native input and a role=checkbox host.
    // Treat those as one control and read the host/label's accessible name.
    const hosts = [...scope.querySelectorAll('mat-checkbox')];
    const raw = [...scope.querySelectorAll('input[type="checkbox"],[role="checkbox"]')];
    const nodes = [...hosts, ...raw.filter(el => !hosts.some(host => host.contains(el)) && !raw.some(other => other !== el && other.contains(el)))];
    const boxes = nodes.map(host => {
      const control = host.matches('input[type="checkbox"]') ? host : host.querySelector('input[type="checkbox"],[role="checkbox"]') || host;
      const labels = [...(control.labels || [])];
      const words = [label(control), label(host), ...labels.map(label)].join(' ').replace(/\s+/g, ' ').trim();
      const click = [host.querySelector('.mdc-checkbox__background,.mat-checkbox-frame,.mat-checkbox-inner-container'), control, ...labels, host].find(visible);
      return {control, host, words, click, checked: control.checked === true || control.getAttribute('aria-checked') === 'true' || host.getAttribute('aria-checked') === 'true'};
    }).filter(box => box.click);
    const optional = /(?:receive|send).*emails?|newsletter|marketing|offers|promotions|news about|updates about|opt.?in|接收.*(?:邮件|消息)|订阅|新聞|新闻|营销|推广|促销|ニュース|メール/i;
    const required = boxes.filter(box => !optional.test(box.words) && /\b18\b|\b(?:age|terms|conditions|acknowledge|agree)\b|年龄|年齡|條款|条款|同意|利用規約|歳/i.test(box.words));
    for (const box of required) {
      if (!box.checked) {
        if (box.control.disabled || box.host.getAttribute('aria-disabled') === 'true') return result('onboarding_wait', 'onboarding');
        return target('onboarding_check', box.click, undefined, 'consent:' + box.words.slice(0, 160));
      }
    }
    const next = named(/^(continue|继续|繼續|agree|同意|続行|次へ)(?:\s*[→›»])?$/i);
    if (required.length && next) return target('onboarding_continue', next);
    return result('onboarding_wait', 'onboarding');
  }
  if (page.origin === 'https://gemini.google.com' && !root.querySelector('rich-textarea, textarea, [contenteditable="true"]')) return result('manual', 'onboarding');
  return result('studio');
})
