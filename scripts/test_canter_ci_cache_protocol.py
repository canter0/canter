import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location(
    "canter_ci_cache_protocol", Path(__file__).with_name("canter-ci-cache.py")
)
CACHE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CACHE)


class CacheProtocolTests(unittest.TestCase):
    def test_origin_fixed_vectors(self):
        cases = (
            ("https://cache.example.com", "https://cache.example.com", "d7a2c78ef18c6d7712d66e426cd7c633d0d543151ea00805410b23944d202211", False),
            ("HTTPS://CACHE.EXAMPLE.COM:0443/", "https://cache.example.com", "d7a2c78ef18c6d7712d66e426cd7c633d0d543151ea00805410b23944d202211", False),
            ("https://cache-staging.example.com/", "https://cache-staging.example.com", "5d4a4117106594d258018ab30f8492ea107e35c5e1775aacfd8edd506c2556c9", False),
            ("https://cache.example.com:8443", "https://cache.example.com:8443", "29eae4a13b91bad0a872bbe4684b4604c4546aaad3f71bf1c040368797117b73", False),
            ("http://127.0.0.1:8080/", "http://127.0.0.1:8080", "a61a3d913c1cb864d0325fb977b8bdd41598fd9cb701c08dab8534b262e9da63", True),
            ("http://[::ffff:127.0.0.1]:8080", "http://127.0.0.1:8080", "a61a3d913c1cb864d0325fb977b8bdd41598fd9cb701c08dab8534b262e9da63", True),
            ("https://[2001:0db8:0:0:0:0:0:1]:443/", "https://[2001:db8::1]", "a26d2992b2738bf482c74c219be8e40e48ee716920a0af0ff192c4e4731a8af7", False),
            ("http://[::1]:8080", "http://[::1]:8080", "2edd7508d9d545d409650be0d678d38e304b22be1ea4de6dae1f0255696b0b67", True),
        )
        for raw, expected, digest, loopback in cases:
            with self.subTest(raw=raw):
                origin, audience = CACHE.cache_origin(raw, allow_loopback_http=loopback)
                self.assertEqual(origin, expected)
                self.assertEqual(audience, "urn:canter:ci:cache:v1:" + digest)

    def test_invalid_origins(self):
        cases = (
            "", "cache.example.com", "https:cache.example.com", "//cache.example.com",
            " https://cache.example.com", "https://cache.example.com\n",
            "https://cache.example.com/a", "https://cache.example.com//",
            "https://cache.example.com/%2f", "https://cache.example.com?",
            "https://cache.example.com?key=x", "https://cache.example.com#",
            "https://cache.example.com/#part", "https://user:password@cache.example.com",
            "https://@cache.example.com", "https://cache.example.com:",
            "https://cache.example.com:0", "https://cache.example.com:65536",
            "https://cache.example.com:bad", "https://cache..example.com",
            "https://cache.example.com.", "https://-cache.example.com",
            "https://cache-.example.com", "https://cache_example.com",
            "https://" + "a" * 64 + ".example.com", "https://caf\u00e9.example.com",
            "https://[fe80::1%25en0]", "http://cache.example.com",
            "http://localhost:8080", "http://192.168.1.1:8080",
            "http://127.000.0.1:8080", "ftp://127.0.0.1:8080",
            "https://cache.example.com\\@evil.example",
        )
        for raw in cases:
            for allow in (False, True):
                with self.subTest(raw=raw, allow=allow):
                    with self.assertRaisesRegex(CACHE.CacheProtocolError, "^invalid cache origin$"):
                        CACHE.cache_origin(raw, allow_loopback_http=allow)
        for raw in ("http://127.0.0.1:8080", "http://[::1]:8080"):
            with self.assertRaises(CACHE.CacheProtocolError):
                CACHE.cache_origin(raw)

    def test_identity_endpoint_binds_local_audience(self):
        _, audience = CACHE.cache_origin("https://cache.example.com")
        self.assertEqual(
            CACHE.github_id_token_url(
                "https://pipelines.actions.githubusercontent.com/job/idtoken?api-version=2.0",
                audience,
            ),
            "https://pipelines.actions.githubusercontent.com/job/idtoken?"
            "api-version=2.0&audience=urn%3Acanter%3Aci%3Acache%3Av1%3A"
            "d7a2c78ef18c6d7712d66e426cd7c633d0d543151ea00805410b23944d202211",
        )

    def test_identity_endpoint_rejects_credential_replay_targets(self):
        _, audience = CACHE.cache_origin("https://cache.example.com")
        cases = (
            "http://pipelines.actions.githubusercontent.com/job/idtoken",
            "https://evil.example/job/idtoken",
            "https://pipelines.actions.githubusercontent.com.evil.example/job/idtoken",
            "https://actions.githubusercontent.com/job/idtoken",
            "https://user:secret@pipelines.actions.githubusercontent.com/job/idtoken",
            "https://pipelines.actions.githubusercontent.com:444/job/idtoken",
            "https://pipelines.actions.githubusercontent.com:/job/idtoken",
            "https://pipelines.actions.githubusercontent.com/job/idtoken#",
            "https://pipelines.actions.githubusercontent.com/job/idtoken?audience=other",
            "https://pipelines.actions.githubusercontent.com/job/idtoken?AUDIENCE=other",
            "https://pipelines.actions.githubusercontent.com/job/idtoken?x",
            "https://pipelines.actions.githubusercontent.com/job/idtoken?x=%ff",
            " https://pipelines.actions.githubusercontent.com/job/idtoken",
            "https://pipelines.actions.githubusercontent.com/job/idtoken\n",
        )
        for raw in cases:
            with self.subTest(raw=raw):
                with self.assertRaisesRegex(CACHE.CacheProtocolError, "^invalid GitHub identity endpoint$"):
                    CACHE.github_id_token_url(raw, audience)
        with self.assertRaises(CACHE.CacheProtocolError):
            CACHE.github_id_token_url(
                "https://pipelines.actions.githubusercontent.com/job/idtoken", "unrelated"
            )


if __name__ == "__main__":
    unittest.main()
