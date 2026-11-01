(() => {
  const referenceConfiguration = {
    sources: [{ url: "/openapi/openapi.yaml" }],
    agent: { disabled: true },
    telemetry: false,
    persistAuth: false,
    withDefaultFonts: false,
    proxyUrl: "",
  };

  const apiReference = window.Scalar.createApiReference(
    "#scalar-reference-root",
    referenceConfiguration,
  );
  const button = document.getElementById("create-access");
  const status = document.getElementById("access-status");
  const initialButtonText = button.textContent;

  // Keep a candidate for this page lifetime so an uncertain response can be
  // retried idempotently without registering a second credential.
  let candidate = null;
  let enrolling = false;
  let enrolled = false;

  function createToken() {
    const bytes = crypto.getRandomValues(new Uint8Array(32));
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    const encoded = btoa(binary)
      .replace(/\+/g, "-")
      .replace(/\//g, "_")
      .replace(/=+$/g, "");
    return `pbt1_${encoded}`;
  }

  async function createCandidate() {
    const token = createToken();
    const digest = await crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode(token),
    );
    const tokenSHA256 = Array.from(new Uint8Array(digest), (byte) =>
      byte.toString(16).padStart(2, "0"),
    ).join("");

    return { token, tokenSHA256 };
  }

  async function enroll() {
    if (enrolling || enrolled) return;

    enrolling = true;
    button.disabled = true;
    button.textContent = "Подключаем…";
    status.textContent = "Подключаем доступ к API…";

    try {
      if (!candidate) candidate = await createCandidate();

      const response = await fetch("/v1/client-installations", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token_sha256: candidate.tokenSHA256 }),
        cache: "no-store",
      });

      if (response.status !== 200 && response.status !== 201) {
        throw new Error("enrollment failed");
      }

      apiReference.updateConfiguration({
        ...referenceConfiguration,
        authentication: {
          preferredSecurityScheme: "bearerAuth",
          securitySchemes: {
            bearerAuth: { token: candidate.token },
          },
        },
      });
      enrolled = true;
      button.textContent = "Подключено";
      status.textContent = "Авторизация подключена.";
    } catch {
      button.disabled = false;
      button.textContent = candidate ? "Повторить" : initialButtonText;
      status.textContent = "Не удалось подключить доступ. Нажмите кнопку, чтобы повторить.";
    } finally {
      enrolling = false;
      if (enrolled) button.disabled = true;
    }
  }

  button.addEventListener("click", enroll);
})();
