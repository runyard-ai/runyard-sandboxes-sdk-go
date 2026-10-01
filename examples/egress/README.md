# A sandbox's egress rules, at work

A sandbox reaches what its rules allow, and nothing else — and its rules change
while it runs, one at a time, by name.

```sh
go run ./examples/egress -key "$RUNYARD_SANDBOXES_KEY"
```

```
making a sandbox from curlimages/curl:8.11.1, with one egress rule: ifconfig allows ifconfig.me
ready: 01a0e5d2-6f0e-7c42-9a1b-3e5d7c9f2a10

$ curl https://ifconfig.me/ip
  203.0.113.7
$ curl https://httpbin.org/ip
  curl: (6) Could not resolve host: httpbin.org
  the host refused httpbin.org, by its resolver: no rule allows it

adding the rule httpbin: httpbin.org, on the web ports
$ curl https://httpbin.org/ip
  {
    "origin": "203.0.113.7"
  }

deleting the rule httpbin
$ curl https://httpbin.org/ip
  curl: (6) Could not resolve host: httpbin.org
  the host refused httpbin.org, by its resolver: no rule allows it

dropped.
```

1. The sandbox is made with one rule, `ifconfig`, which allows `ifconfig.me`
   on the web ports.
2. `curl https://ifconfig.me/ip` is reached. What it prints is the address the
   sandbox left through: its host's, which masquerades for it.
3. `curl https://httpbin.org/ip` is refused before it starts: no rule allows
   the name, so the sandbox's resolver — on the host — does not answer it. The
   sandbox's egress report (`GET .../egress`) has the refusal, and so does the
   console's Network tab, with a button that adds the rule.
4. `PUT .../egress/rules/httpbin` adds a rule while the machine runs. It
   answers once the host's firewall enforces it, so the next curl is reached.
5. `DELETE .../egress/rules/httpbin` takes that rule away, and only that one:
   `ifconfig` is untouched, and httpbin.org is refused again.

It checks what it shows: a site a rule allows that is not reached, or one no
rule allows that is, fails the run rather than printing a tour that looks
right. The sandbox is dropped in either case, and on Ctrl-C.

It needs a daemon that gives sandboxes a network, which is the default, and a
key with `network.write`. `-image` takes any image with `curl` and a shell.
