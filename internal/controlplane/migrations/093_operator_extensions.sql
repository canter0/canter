ALTER TABLE wren_extension_installations DROP CONSTRAINT IF EXISTS wren_extension_installations_scope_kind_check;
ALTER TABLE wren_extension_installations ADD CONSTRAINT wren_extension_installations_scope_kind_check CHECK(scope_kind IN ('private','room','operator'));
ALTER TABLE wren_extension_installations ADD CONSTRAINT wren_extension_installations_operator_wren_check CHECK(scope_kind<>'operator' OR wren_id IS NULL);
