# Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License"). You may
# not use this file except in compliance with the License. A copy of the
# License is located at
#
#	http://aws.amazon.com/apache2.0/
#
# or in the "license" file accompanying this file. This file is distributed
# on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
# express or implied. See the License for the specific language governing
# permissions and limitations under the License.
.PHONY: all tar image mockgen clean

CSI_DRIVER_VERSION="v$(shell cat CSI_DRIVER_VERSION)"
BUILD_DATE=$(shell date -u -Iseconds)
GO_VERSION=$(shell cat ../../../GO_VERSION)

all: tar

image:
	docker build --build-arg GO_VERSION=$(GO_VERSION) --build-arg CSI_DRIVER_VERSION=$(CSI_DRIVER_VERSION) --build-arg BUILD_DATE="$(BUILD_DATE)" -t aws-ebs-csi-driver:make .

tar: image
	mkdir tarfiles
	docker save "aws-ebs-csi-driver:make" > tarfiles/aws-ebs-csi-driver.tar

mockgen:
	./update-gomock

clean:
	-docker rmi -f "aws-ebs-csi-driver:make"
	-rm -rf tarfiles
