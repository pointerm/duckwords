# Third-Party Notices

This document covers third-party material compiled into DuckWords, copied into its
runtime container, or fetched by its default runtime configuration. It does not
grant a license to the DuckWords source code itself.

## Go standard library and `golang.org/x/sync`

The statically linked DuckWords binary contains:

- the Go 1.26.6 standard library ([source](https://go.dev/)); and
- `golang.org/x/sync` v0.22.0
  ([source](https://github.com/golang/sync/tree/v0.22.0)).

Both components use the following BSD 3-Clause license. The authoritative Go
standard-library license is available at <https://go.dev/LICENSE>; the license at
the pinned `x/sync` tag is available at
<https://github.com/golang/sync/blob/v0.22.0/LICENSE>.

```text
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

## Alpine Mozilla CA certificate bundle

The scratch runtime image copies only the generated
`/etc/ssl/certs/ca-certificates.crt` trust bundle from the pinned
`golang:1.26.6-alpine3.23` builder. That builder contains Alpine package
`ca-certificates-bundle` 20260611-r0, described by Alpine as a pre-generated
bundle of Mozilla certificates and licensed under **MPL-2.0 AND MIT**. Both
licenses therefore apply; `AND` is not a choice between them.

- Alpine package metadata and source links:
  <https://pkgs.alpinelinux.org/package/v3.23/main/x86_64/ca-certificates-bundle>
- Mozilla Public License 2.0: <https://www.mozilla.org/MPL/2.0/>
- MIT license text: <https://spdx.org/licenses/MIT.html>
- Mozilla CA Certificate Program:
  <https://www.mozilla.org/en-US/about/governance/policies/security-group/certs/>

DuckWords does not modify the certificates. Certificate inclusion in the trust
bundle does not itself endorse DuckWords or change a certificate owner's terms.

## Runtime-fetched English word bank

The default word-bank URL points to `dwyl/english-words`:
<https://raw.githubusercontent.com/dwyl/english-words/master/words.txt>. The word
bank is fetched at runtime and is not bundled in the DuckWords repository, binary,
or container image. Its upstream repository publishes the data under the Unlicense:
<https://github.com/dwyl/english-words/blob/master/LICENSE.md>.
