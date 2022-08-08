#!/bin/sh

sudo pt-query-digest $1 --report-format=query_report --limit=6 --filter='$event->{arg} =~ m/^select/i'
