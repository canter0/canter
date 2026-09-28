#!/usr/bin/env python3
"""Verify the public font assets referenced by Canter's stylesheet."""
import argparse
import json
from pathlib import Path
import re
import struct
import urllib.request

STYLESHEET = Path(__file__).resolve().parents[1] / 'web/src/app/globals.css'


def check_fonts(base_url='https://canter.dev', stylesheet=STYLESHEET):
    paths = sorted(set(re.findall(r'url\([\"\']?(/fonts/[^\s)\"\']+)[\"\']?\)',
                                  stylesheet.read_text())))
    if not paths:
        raise ValueError('No site font URLs found in the stylesheet')
    results = {}
    for path in paths:
        try:
            request = urllib.request.Request(base_url.rstrip('/') + path,
                                             headers={'User-Agent': 'canter-font-check',
                                                      'Cache-Control': 'no-cache'})
            with urllib.request.urlopen(request, timeout=10) as response:
                data = response.read()
                if len(data) < 48 or data[:4] != b'wOF2':
                    raise ValueError('Response is not a WOFF2 font')
                if struct.unpack_from('>I', data, 8)[0] != len(data):
                    raise ValueError('WOFF2 file is truncated or has an invalid length')
                results[path] = {'healthy': True, 'bytes': len(data)}
        except (OSError, ValueError) as exc:
            results[path] = {'healthy': False, 'error': str(exc)}
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', default='https://canter.dev')
    args = parser.parse_args()
    results = check_fonts(args.base_url)
    print(json.dumps(results, indent=2))
    return 0 if all(font['healthy'] for font in results.values()) else 1


if __name__ == '__main__':
    raise SystemExit(main())
