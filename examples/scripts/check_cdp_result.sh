#!/bin/bash
ssh -o ConnectTimeout=5 login07 "grep -E 'size:|===|Sizes|URL|ok' ~/cdp_result.log 2>&1 | tail -25"
