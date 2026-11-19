// Readable device label from a session's user agent. Order matters: Edge and
// Chrome both appear in the same string, and iOS/Android UAs also contain
// "Mac OS X"/"Linux", so the more specific tokens win first.
export function sessionDeviceLabel(userAgent: string): string {
  const ua = userAgent ?? "";
  let browser = "";
  if (/Edg(e|A|iOS)?\//.test(ua)) browser = "Edge";
  else if (/FxiOS\/|Firefox\//.test(ua)) browser = "Firefox";
  else if (/CriOS\/|Chrome\/|Chromium\//.test(ua)) browser = "Chrome";
  else if (/Safari\//.test(ua)) browser = "Safari";
  let device = "";
  if (/iPad/.test(ua)) device = "iPad";
  else if (/iPhone|iPod/.test(ua)) device = "iPhone";
  else if (/Android/.test(ua)) device = "Android";
  else if (/CrOS/.test(ua)) device = "ChromeOS";
  else if (/Windows/.test(ua)) device = "Windows";
  else if (/Macintosh|Mac OS X/.test(ua)) device = /Mobile\//.test(ua) ? "iPad" : "Mac";
  else if (/Linux|X11/.test(ua)) device = "Linux";
  if (browser && device) return `${browser} on ${device}`;
  if (browser) return browser;
  if (device) return `Browser on ${device}`;
  return "Browser session";
}
