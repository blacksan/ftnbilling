-- Optional GPS point of an apartment (decimal degrees, WGS84). Both set or both NULL.
-- Forward-only; never edit once released.
SET LOCAL lock_timeout = '10s';

ALTER TABLE apartments ADD COLUMN lat double precision, ADD COLUMN lng double precision,
  ADD CONSTRAINT apartments_gps_shape CHECK ((lat IS NULL AND lng IS NULL)
    OR (lat IS NOT NULL AND lng IS NOT NULL AND lat BETWEEN -90 AND 90 AND lng BETWEEN -180 AND 180));
