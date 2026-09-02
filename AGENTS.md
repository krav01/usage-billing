# Project instructions

Always load `samber/cc-skills-golang@golang-how-to` for Go work and its relevant secondary skills.

This is a portfolio-only usage billing service, not a payment processor.
Use compact packages and manual constructor injection. Do not add an ORM or DI framework.
The user explicitly approved generating SQL schema/migrations for this isolated educational database
and adding `github.com/jackc/pgx/v5`. This overrides the database skill's schema-generation prohibition
only for this new project. Never connect to or migrate an existing user or production database.
Use synthetic test data. Never commit secrets, real customer data, or unmeasured performance claims.
New dependencies beyond pgx require confirmation. Keep shared contracts stable; coordinate changes.
