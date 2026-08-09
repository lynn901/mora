-- migrations/012_rotate_demo_token.down.sql
UPDATE api_tokens
SET token_hash = '15611f355a858b9800b308d515aaaba205a0859283ad42af86578894da069a07',
    prefix = 'wki_dev_a1b2'
WHERE token_hash = 'd2e038407eb0a334decd26d3b91d88acfaa23fbfdfe6c1008927e05d416f5f2e';
