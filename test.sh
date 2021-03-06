#!/bin/bash
set -exo pipefail

if [[ -z "$TEST_RESULTS" ]]; then
  TEST_RESULTS=/tmp/test-results
fi

mkdir -p $TEST_RESULTS
go get -u -v github.com/jstemmer/go-junit-report
export PATH=$GOPATH/bin:$PATH
make test | tee ${TEST_RESULTS}/go-test.out
$GOPATH/bin/go-junit-report <${TEST_RESULTS}/go-test.out > ${TEST_RESULTS}/go-test-report.xml

make vet

make lint
