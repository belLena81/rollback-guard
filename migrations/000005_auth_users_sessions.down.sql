-- Lock first so no row can be inserted between the check and the drop.
LOCK TABLE users, sessions IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM sessions) THEN
        RAISE EXCEPTION 'cannot remove auth migration while users or sessions exist';
    END IF;
END
$$;

DROP TABLE sessions;
DROP TABLE users;
