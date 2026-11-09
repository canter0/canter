ALTER TABLE wren_responsibilities ADD COLUMN IF NOT EXISTS report_policy text NOT NULL DEFAULT 'always' CHECK(report_policy IN ('always','changes_only'));
-- Older accepted wakes did not record a policy; do not invent historical consent.
ALTER TABLE wren_responsibility_wakes ADD COLUMN IF NOT EXISTS report_policy text CHECK(report_policy IN ('always','changes_only'));
