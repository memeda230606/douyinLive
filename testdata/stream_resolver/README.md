# Stream resolver fixtures

These fixtures are minimal, synthetic derivatives of the response shape in
'doRequest.example.json'. They intentionally retain only fields needed by the
stream resolver tests.

Safety rules:

- Stream hosts use the reserved '.invalid' top-level domain.
- URLs contain no query strings, signatures, tokens, cookies, room IDs, or user
  data.
- Nested JSON strings remain JSON strings so parser coverage matches the
  upstream response shape.
- A fixture must pass 'TestStreamResolverFixturesAreSanitizedAndWellFormed'
  before it is committed.
