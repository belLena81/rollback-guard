DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM sessions) THEN
        RAISE EXCEPTION 'cannot remove auth migration while users or sessions exist';
    END IF;
END
$$;

DROP TABLE sessions;
DROP TABLE users;
