ALTER TABLE systems ADD COLUMN IF NOT EXISTS m1_prefix text;

UPDATE systems
SET m1_prefix = contract #>> '{spec,m1,prefix}'
WHERE m1_prefix IS NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM systems
        WHERE m1_prefix IS NULL
           OR length(m1_prefix) > 512
           OR m1_prefix !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}(/[A-Za-z0-9][A-Za-z0-9._-]{0,127})*$'
    ) THEN
        RAISE EXCEPTION 'existing System has an unsafe m1 prefix; repair it and move its stored objects explicitly before retrying migration';
    END IF;
    IF EXISTS (
        SELECT m1_prefix FROM systems GROUP BY m1_prefix HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'multiple existing Systems share an m1 prefix; resolve ownership explicitly before retrying migration';
    END IF;
END $$;

ALTER TABLE systems ALTER COLUMN m1_prefix SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS systems_m1_prefix_key
    ON systems (m1_prefix);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'systems_m1_prefix_contract_check'
          AND conrelid = 'systems'::regclass
    ) THEN
        ALTER TABLE systems ADD CONSTRAINT systems_m1_prefix_contract_check
            CHECK (m1_prefix = contract #>> '{spec,m1,prefix}');
    END IF;
END $$;
