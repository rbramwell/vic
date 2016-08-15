#!/bin/bash
# Copyright 2016 VMware, Inc. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
upload_logs() {
    timestamp=$(date +%s)
    outfile="integration_logs_"$DRONE_BUILD_NUMBER"_"$DRONE_COMMIT"_$timestamp.tar"

    tar cf $outfile log.html package.list *container-logs.zip *.log

    # GC credentials
    set +x
    keyfile="/root/vic-ci-logs.key"
    botofile="/root/.boto"
    echo -en $GS_PRIVATE_KEY > $keyfile
    chmod 400 $keyfile
    echo "[Credentials]" >> $botofile
    echo "gs_service_key_file = $keyfile" >> $botofile
    echo "gs_service_client_id = $GS_CLIENT_EMAIL" >> $botofile
    echo "[GSUtil]" >> $botofile
    echo "content_language = en" >> $botofile
    echo "default_project_id = $GS_PROJECT_ID" >> $botofile
    set -x

    if [ -f "$outfile" ]; then
      gsutil cp $outfile gs://vic-ci-logs
      loglink="https://console.cloud.google.com/m/cloudstorage/b/vic-ci-logs/o/$outfile?authuser=1"
      echo "Download test logs:"
      echo $loglink
    else
      echo "No log output file to upload"
    fi

    if [ -f "$keyfile" ]; then
      rm -f $keyfile
    fi
}

set -x

gsutil version -l

dpkg -l > package.list

if [ $DRONE_BRANCH = "integration/*" ]; then
    $rc = pybot --removekeywords TAG:secret tests/test-cases
    upload_logs
else
    $rc = pybot --removekeywords TAG:secret --include regression tests/test-cases
    upload_logs
    if [ $rc = 0 ]; then
        git clone https://github.com/vmwware/vic integration
        cd integration
        git checkout -b integration/$DRONE_BUILD
        $patch = curl -s https://api.github.com/repos/vmware/vic/pulls/$DRONE_PULL_REQUEST | jq -r .patch_url
        wget $patch
        git apply $DRONE_PULL_REQUEST.patch
        git add .
        git commit -m"Automated integration test branch"
        git push origin integration/$DRONE_BUILD
        curl --user "vmware-vic:$GITHUB_AUTOMATION_API_KEY" -X POST --data '{"title":"Integration automation created","head":"integration/'$DRONE_BUILD'","base":"master"}'    https://api.github.com/repos/vmware/vic/pulls
    fi
fi

exit $rc
