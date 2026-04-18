-- 0015_image_validation_results.down.sql
--
-- Reverses 0015_image_validation_results.up.sql.

DROP TABLE IF EXISTS image_cve_waivers;
DROP TABLE IF EXISTS image_validation_results;
DROP TABLE IF EXISTS image_build_manifests;
