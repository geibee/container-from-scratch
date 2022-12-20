WORKDIR=$(pwd)
if ls runc_workspace/runc/rootfs > /dev/null 2>&1; then
    echo "rootfs" exists
else
    mkdir -p runc_workspace/runc/bundle/rootfs
    docker run --rm -d --name ubuntu ubuntu:18.04 tail -f /dev/null
    docker export ubuntu > rootfs.tar
    docker kill ubuntu
    tar xf rootfs.tar -C runc_workspace/runc/bundle/rootfs
    rm rootfs.tar
fi

cd runc_workspace/runc
make
cd $WORKDIR
mv runc_workspace/runc/runc $WORKDIR

rm -f runc_workspace/runc/bundle/config.json
sudo ./runc spec -b runc_workspace/runc/bundle
# sudo sh -c "jq --tab '.process.terminal|=false' runc_workspace/runc/bundle/config.json > runc_workspace/runc/bundle/config2.json"
# sudo sh -c "cat runc_workspace/runc/bundle/config2.json | jq --tab . > runc_workspace/runc/bundle/config.json"
# rm -f runc_workspace/runc/bundle/config2.json
sudo ./runc run test-ubuntu -b runc_workspace/runc/bundle 