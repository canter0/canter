#!/usr/bin/env python3
"""Create/reuse the Stripe catalog for Canter. Test keys only unless --live.

This creates billing configuration, not customers, subscriptions, or charges.
Outputs public configuration IDs only. Secret keys remain in the environment.
"""
import argparse
import json
import os
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

API_VERSION = "2025-03-31.basil"
CATALOG = json.loads((Path(__file__).resolve().parents[1] / "pricing/catalog.json").read_text())
PRO = next(plan for plan in CATALOG["plans"] if plan["id"] == "pro")
EVENT = "canter_usage_cents_v1"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", action="store_true", help="Explicitly allow creating the live catalog")
    parser.add_argument("--payg-only", action="store_true", help="Only configure pay as you go")
    args = parser.parse_args()
    secret = os.environ.get("CANTER_STRIPE_SECRET_KEY", "")
    if not secret:
        parser.error("Set CANTER_STRIPE_SECRET_KEY in the environment.")
    if not args.live and not secret.startswith(("sk_test_", "rk_test_")):
        parser.error("Use a Stripe test key, or explicitly pass --live.")

    def api(method, path, params=None, key=None):
        encoded = urlencode(params or {}, doseq=True)
        address = "https://api.stripe.com/v1/" + path
        if method == "GET" and encoded:
            address += "?" + encoded
        request = Request(address, data=encoded.encode() if method == "POST" else None, method=method)
        request.add_header("Authorization", "Bearer " + secret)
        request.add_header("Stripe-Version", API_VERSION)
        request.add_header("Content-Type", "application/x-www-form-urlencoded")
        if key:
            request.add_header("Idempotency-Key", key)
        try:
            with urlopen(request, timeout=20) as response:
                return json.load(response)
        except HTTPError as error:
            if method == "GET" and error.code == 404:
                return None
            raise SystemExit("Stripe returned HTTP " + str(error.code) + " for " + path + ". No secrets were logged.") from None

    product_id = "canter_hosting_v1"
    if not api("GET", "products/" + product_id):
        api("POST", "products", {"id": product_id, "name": "Canter hosting", "description": "Compute and object storage. Pro costs $20 per month and includes $20 of resource usage; pay as you go has no subscription fee."}, "canter-product-v1")
    meter = next((item for item in api("GET", "billing/meters", {"status": "active", "limit": 100})["data"] if item["event_name"] == EVENT), None)
    if not meter:
        meter = api("POST", "billing/meters", {"display_name": "Canter rated usage (USD cents)", "event_name": EVENT, "default_aggregation[formula]": "sum", "customer_mapping[type]": "by_id", "customer_mapping[event_payload_key]": "stripe_customer_id", "value_settings[event_payload_key]": "value"}, "canter-meter-v1")

    def price(lookup, properties):
        existing = api("GET", "prices", {"lookup_keys[]": lookup, "active": "true"})["data"]
        if existing:
            return existing[0]["id"]
        params = {"product": product_id, "currency": CATALOG["currency"], "recurring[interval]": "month", "lookup_key": lookup, "tax_behavior": "exclusive", **properties}
        return api("POST", "prices", params, lookup)["id"]

    payg = price("canter_payg_v1", {"unit_amount": 1, "recurring[usage_type]": "metered", "recurring[meter]": meter["id"]})
    base = overage = ""
    if not args.payg_only:
        base = price("canter_pro_base_v1", {"unit_amount": PRO["monthlyCents"], "recurring[usage_type]": "licensed"})
        overage = price("canter_pro_usage_v1", {"billing_scheme": "tiered", "tiers_mode": "graduated", "recurring[usage_type]": "metered", "recurring[meter]": meter["id"], "tiers[0][up_to]": PRO["includedUsageCents"], "tiers[0][unit_amount]": 0, "tiers[1][up_to]": "inf", "tiers[1][unit_amount]": 1})
    # The dedicated portal configuration prevents mid-cycle plan changes from
    # accidentally resetting the included allowance. Cancel at period end only.
    portal = next((item for item in api("GET", "billing_portal/configurations", {"active": "true", "limit": 100})["data"] if item.get("metadata", {}).get("canter_catalog") == "v1"), None)
    if not portal:
        portal = api("POST", "billing_portal/configurations", {"business_profile[headline]": "Manage your Canter billing", "features[payment_method_update][enabled]": "true", "features[invoice_history][enabled]": "true", "features[subscription_cancel][enabled]": "true", "features[subscription_cancel][mode]": "at_period_end", "features[subscription_update][enabled]": "false", "metadata[canter_catalog]": "v1"}, "canter-portal-v1")
    for key, value in {"CANTER_STRIPE_PAYG_PRICE_ID": payg, "CANTER_STRIPE_PRO_PRICE_ID": base, "CANTER_STRIPE_PRO_USAGE_PRICE_ID": overage, "CANTER_STRIPE_METER_ID": meter["id"], "CANTER_STRIPE_METER_EVENT_NAME": EVENT, "CANTER_STRIPE_PORTAL_CONFIGURATION_ID": portal["id"]}.items():
        print(key + "=" + value)
    print("# Keep CANTER_BILLING_ENABLED=false until webhook delivery and your rated usage producer are verified.")


if __name__ == "__main__":
    main()
