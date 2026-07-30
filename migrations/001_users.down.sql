-- migrations/001_users.down.sql
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
DROP TABLE IF EXISTS service_accounts;
DROP TABLE IF EXISTS users;
DROP TEXT SEARCH CONFIGURATION IF EXISTS chinese_zh;
