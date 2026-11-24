import { Client, ApiError } from "./out/index.js";

const base = process.argv[2];
const api = new Client({ baseUrl: base });
let failed = 0;
const check = (name, ok, detail = "") => {
  console.log((ok ? "ok   " : "FAIL ") + name + (ok ? "" : "  " + detail));
  if (!ok) failed++;
};

const hello = await api.hello.get({ query: { name: "ts" } });
check("hello", hello.message === "Hello, ts!", JSON.stringify(hello));

const user = await api.users.create({ body: { name: "Ada", email: "ada@example.com" } });
check("create user (json body, defaults)", user.id > 0 && user.role === "member", JSON.stringify(user));

const got = await api.users.get({ path: { id: user.id } });
check("get user (path param)", got.name === "Ada");

const patched = await api.users.update({ path: { id: user.id }, body: { role: "admin" } });
check("patch user", patched.role === "admin" && patched.name === "Ada", JSON.stringify(patched));

const list = await api.users.list({ query: { limit: 5 } });
check("list users (query param)", Array.isArray(list) && list.length >= 1);

try {
  await api.users.create({ body: { name: "", email: "nope" } });
  check("validation error throws", false, "no error");
} catch (e) {
  check("validation error throws ApiError 422", e instanceof ApiError && e.status === 422, String(e));
}

const upload = await api.upload.create({ body: { file: new Blob(["hello upload"]), title: "Notes" } });
check("multipart upload", upload.bytes === 12 && upload.title === "Notes", JSON.stringify(upload));

const file = await api.files.get({ path: { path: "a/b.txt" } });
check("catch-all path param", (await file.text()) === "a/b.txt");

await api.users.delete({ path: { id: user.id } });
try {
  await api.users.get({ path: { id: user.id } });
  check("deleted user is gone", false, "no error");
} catch (e) {
  check("deleted user is a 404 ApiError", e instanceof ApiError && e.status === 404, String(e));
}

const authed = new Client({ baseUrl: base, headers: async () => ({ authorization: "Bearer dev-token" }) });
const stats = await authed.admin.stats();
check("bearer header via the headers option", typeof stats.users === "number", JSON.stringify(stats));
try {
  await api.admin.stats();
  check("admin without a token throws", false, "no error");
} catch (e) {
  check("admin without a token is a 401", e instanceof ApiError && e.status === 401, String(e));
}

console.log(failed ? "FAILED" : "ALL OK");
process.exit(failed ? 1 : 0);
