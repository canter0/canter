import io
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import HTTPError

from check_fonts import check_fonts


class FontAssetsTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.stylesheet = Path(self.tmp.name) / 'fonts.css'
        self.stylesheet.write_text('@font-face { src: url("/fonts/display.woff2"); }')
        self.font = b'wOF2' + b'\x00\x01\x00\x00' + struct.pack('>I', 48) + bytes(36)

    def check(self):
        return check_fonts(stylesheet=self.stylesheet)['/fonts/display.woff2']

    @patch('check_fonts.urllib.request.urlopen')
    def test_valid_font(self, urlopen):
        urlopen.return_value = io.BytesIO(self.font)
        self.assertEqual(self.check(), {'healthy': True, 'bytes': 48})

    @patch('check_fonts.urllib.request.urlopen')
    def test_missing_font(self, urlopen):
        urlopen.side_effect = HTTPError('https://canter.dev/fonts/display.woff2', 404,
                                       'Not Found', {}, None)
        self.assertFalse(self.check()['healthy'])

    @patch('check_fonts.urllib.request.urlopen')
    def test_html_success_and_truncated_font_fail(self, urlopen):
        for content in [b'<!doctype html>' * 20, self.font[:-1], self.font + b'extra']:
            with self.subTest(content=content):
                urlopen.return_value = io.BytesIO(content)
                self.assertFalse(self.check()['healthy'])

    @patch('check_fonts.urllib.request.urlopen')
    def test_checks_each_distinct_font(self, urlopen):
        self.stylesheet.write_text('url("/fonts/display.woff2") url(/fonts/mono.woff2) '
                                   "url('/fonts/display.woff2')")
        urlopen.side_effect = [io.BytesIO(self.font), io.BytesIO(self.font)]
        self.assertEqual(len(check_fonts(stylesheet=self.stylesheet)), 2)
        self.assertEqual(urlopen.call_count, 2)

    def test_empty_font_list_cannot_pass(self):
        self.stylesheet.write_text('body { font-family: sans-serif; }')
        with self.assertRaisesRegex(ValueError, 'No site font'):
            check_fonts(stylesheet=self.stylesheet)


if __name__ == '__main__':
    unittest.main()
