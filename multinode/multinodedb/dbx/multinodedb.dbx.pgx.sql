-- AUTOGENERATED BY storj.io/dbx
-- DO NOT EDIT
CREATE TABLE members (
	id bytea NOT NULL,
	email text NOT NULL,
	name text NOT NULL,
	password_hash bytea NOT NULL,
	created_at timestamp with time zone NOT NULL,
	PRIMARY KEY ( id )
);
CREATE TABLE nodes (
	id bytea NOT NULL,
	name text NOT NULL,
	public_address text NOT NULL,
	api_secret bytea NOT NULL,
	PRIMARY KEY ( id )
);
