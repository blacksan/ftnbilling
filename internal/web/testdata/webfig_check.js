// Browser check of remote WebFig against a fake WebFig-like router page, through the real handler.
// Run by TestWebFigInABrowser (WEBFIG_PLAYWRIGHT=1). Usage: node webfig_check.js <open-url> <host:port>
const { chromium } = require('playwright');
const [, , openURL, hostPort] = process.argv;
const fail = (m) => { console.error('FAIL: ' + m); process.exitCode = 1; };
const ok = (c, m) => { if (!c) fail(m); else console.log('ok: ' + m); };
(async () => {
  const browser = await chromium.launch({
    executablePath: process.env.CHROME_BIN || '/usr/bin/google-chrome',
    args: ['--no-sandbox', '--no-proxy-server', '--host-resolver-rules=MAP tech.example.co.ke 127.0.0.1'],
  });
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await ctx.newPage();
  const base = 'https://' + hostPort;

  // 1. no session: the plain page, nothing from the router
  let r = await page.goto(base + '/webfig/');
  ok(r.status() === 401 && (await page.textContent('body')).includes('Open WebFig from the router page in the admin'), 'no session shows the plain page');

  // 2. the ticket opens WebFig: redirect to /, the router's page appears
  r = await page.goto(openURL);
  ok(page.url() === base + '/', 'ticket redirects to the root of the WebFig host (' + page.url() + ')');
  ok((await page.textContent('body')).includes('RouterOS WebFig on 10.99.0.2:8081'), 'the router page is served');
  const h = r.headers();
  ok(h['x-frame-options'] === 'DENY' && h['cache-control'] === 'no-store' && h['referrer-policy'] === 'no-referrer', 'security headers on proxied responses ' + JSON.stringify(h));

  // 3. the page's own script talks to /jsproxy and gets the echo (absolute paths work)
  await page.goto(base + '/webfig/');
  await page.click('#go');
  await page.waitForFunction(() => document.querySelector('#out').textContent.startsWith('echo:'));
  ok((await page.textContent('#out')) === 'echo:hello-jsproxy', '/jsproxy POST round trip');

  // 4. cookie scope
  const cs = (await ctx.cookies()).filter((c) => c.name.endsWith('ftn_wf'));
  ok(cs.length === 1 && cs[0].name === '__Host-ftn_wf' && cs[0].secure && cs[0].httpOnly && cs[0].sameSite === 'Strict' && cs[0].path === '/' && !cs[0].domain.startsWith('.'), 'session cookie is host-only, Secure, HttpOnly, Strict');
  ok(!(await page.evaluate(() => document.cookie)).includes('ftn_wf'), 'the session cookie is not readable by page scripts');

  // 5. the ticket is single use, also in a fresh browser
  const ctx2 = await browser.newContext({ ignoreHTTPSErrors: true });
  const p2 = await ctx2.newPage();
  r = await p2.goto(openURL);
  ok(r.status() === 403 && (await p2.textContent('body')).includes('already used'), 'a used ticket is refused');

  // 6. cannot be framed
  await page.setContent('<iframe id="f" src="' + base + '/"></iframe>');
  await page.waitForTimeout(500);
  const framed = await page.evaluate(() => { try { return document.getElementById('f').contentDocument && document.getElementById('f').contentDocument.body.textContent.includes('WebFig'); } catch (e) { return false; } });
  ok(!framed, 'the WebFig host refuses to be framed');
  await browser.close();
})().catch((e) => { fail(String(e)); process.exit(1); });
