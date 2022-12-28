mkdir -p tmp
mkdir -p rootfs
docker run --rm -d --name ubuntu ubuntu:18.04 tail -f /dev/null
docker export ubuntu > rootfs.tar
docker kill ubuntu
docker container prune -f
tar xf rootfs.tar -C rootfs
rm rootfs.tar

str="$(IFS=,; echo "${@}")"
go build -o cfs -tags=linux
sudo ./cfs run --command "$str"
rm -f ./cfs
rm -rf ./rootfs
rm -f tmp/exec.fifo 
