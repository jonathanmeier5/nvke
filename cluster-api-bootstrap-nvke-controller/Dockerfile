# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Built from the repository root, because this module depends on agent/ through
# a replace directive: the gRPC client for nvke-agent lives there and is not
# published, so the agent module has to be in the build context.
#
# GOWORK=off keeps go.work out of it. The workspace names a dozen other modules
# that are not in the context, and resolving them would fail; the replace
# directive is what this build needs, and that lives in go.mod.
FROM golang:1.27 AS build
ENV GOWORK=off
WORKDIR /src
COPY agent/ agent/
WORKDIR /src/cluster-api-bootstrap-nvke-controller
COPY cluster-api-bootstrap-nvke-controller/go.mod cluster-api-bootstrap-nvke-controller/go.sum ./
RUN go mod download
COPY cluster-api-bootstrap-nvke-controller/api/ api/
COPY cluster-api-bootstrap-nvke-controller/internal/ internal/
COPY cluster-api-bootstrap-nvke-controller/cmd/ cmd/
RUN CGO_ENABLED=0 go build -o /manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
