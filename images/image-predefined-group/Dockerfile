# Copyright 2018 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM busybox

# User default-user(uid=1000) belongs to
# group-defined-in-image(gid=50000) group in this image.
RUN adduser -u 1000 -D default-user && \
    addgroup -g 50000 group-defined-in-image && \
    addgroup default-user group-defined-in-image

# Default User of the image is default-user(uid=1000)
USER 1000
