-- Read-only first-touch cohort report. Run with psql against Canter's database.
-- Visitors are new anonymous browsers within a 30-day cookie window, not people
-- or sessions. Counts depend on cookies/JavaScript and exclude DNT/GPC opt-outs.
-- Conversions belong to the landing-date cohort and may happen after that day.
-- Gross paid USD includes taxes and does not subtract refunds or costs.
SELECT source,landing_path,
    sum(visitors) AS visitors,
    sum(signups) AS signups,
    sum(connected_agents) AS connected_agents,
    sum(deployed_workspaces) AS deployed_workspaces,
    sum(usage_workspaces) AS usage_workspaces,
    sum(paid_workspaces) AS paid_workspaces,
    round(sum(gross_paid_usd_cents)/100.0,2) AS gross_paid_usd
FROM acquisition_funnel_daily
WHERE day >= (now() AT TIME ZONE 'UTC')::date - 29
GROUP BY source,landing_path
ORDER BY deployed_workspaces DESC,signups DESC,visitors DESC;
