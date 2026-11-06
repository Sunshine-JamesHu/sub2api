(() => {
  const emailPattern = /[A-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi;
  const normalize = value => typeof value === 'string' ? value.trim().toLowerCase() : '';
  const validEmail = value => {
    const matches = value.match(emailPattern) || [];
    return matches.length === 1 && matches[0] === value;
  };
  let email = normalize(window.WIZ_global_data?.oPEP7c);
  let identity = validEmail(email) ? 'page-data' : 'missing';
  if (identity === 'missing') {
    email = '';
    const candidates = new Set();
    // Account controls are usable even when WIZ_global_data does not expose
    // the email. Never scan body text: prompts and model replies contain emails.
    for (const element of document.querySelectorAll('a[href], button[aria-label], [role="button"][aria-label]')) {
      if (element.closest('ms-chat-turn, ms-prompt-box, ms-markdown-renderer, textarea, [contenteditable="true"]')) continue;
      if (!element.checkVisibility({visibilityProperty: true})) continue;
      const label = element.getAttribute('aria-label') || '';
      let accountLink = false;
      try {
        const url = new URL(element.getAttribute('href'), location.href);
        const accountControl = element.getAttribute('role') === 'button' || element.closest('header, [role="banner"]');
        accountLink = !!accountControl && url.protocol === 'https:' && (
          (url.hostname === 'accounts.google.com' && url.pathname === '/SignOutOptions') ||
          url.hostname === 'myaccount.google.com'
        );
      } catch {}
      const accountLabel = /^Google\s*(?:Account|账号|帐号|账户|帐户)\s*[:：]/i.test(label);
      if (!accountLink && !accountLabel) continue;
      const values = [label, element.getAttribute('title') || ''];
      if (accountLink) values.push(element.textContent || '');
      const matches = [...new Set(values.flatMap(value => (value.match(emailPattern) || []).map(normalize)))];
      // A control containing several addresses is not a verified selection.
      if (matches.length > 1) { candidates.add('ambiguous'); continue; }
      if (matches.length === 1) candidates.add(matches[0]);
    }
    if (candidates.size === 1 && !candidates.has('ambiguous')) {
      email = [...candidates][0];
      identity = 'account-control';
    } else if (candidates.size > 0) {
      identity = 'ambiguous';
    }
  }
  let storage = [];
  try { storage = Object.entries(localStorage).map(([name, value]) => ({name, value})); } catch {}
  return JSON.stringify({origin: location.origin, email, identity, storage});
})()
