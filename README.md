# Anodyne-API

Anodyne's API backend.

## run
### with nix
`nix develop`

### without nix
```shell
go build .
tmux new-session -d -s anodyne-api './anodyne-api -db /anodyne-frontend/db.sqlite'
```

### options
- `-db <path>`: path to the SQLite database (default `db.sqlite`)
- `-port <n>`: port to listen on (default `8080`)
