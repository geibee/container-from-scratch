rm tmp/exec.fifo
go build -o cfs 
sudo ./cfs run --command "/bin/echo hello"
