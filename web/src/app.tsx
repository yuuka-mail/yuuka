import { useSession } from "@/api/queries";
import { Logo } from "@/components/logo";
import { Skeleton } from "@/components/ui/skeleton";
import { useLocation } from "@/lib/router";
import { MailShell } from "@/mail/shell";
import { EnrollPage } from "@/pages/enroll";
import { LoginPage } from "@/pages/login";
import { RecoverPage } from "@/pages/recover";

/**
 * The page shell needs no session: a link from mail or Telegram is a
 * cross-site navigation, so the SameSite=Strict cookie is not sent with
 * it. The shell loads, then asks /auth/session with a same-origin fetch,
 * which does carry the cookie.
 */
export function App() {
  const { pathname } = useLocation();
  switch (pathname) {
    case "/enroll":
      return <EnrollPage />;
    case "/recover":
      return <RecoverPage />;
    case "/login":
      return <LoginPage />;
  }
  return <Gate />;
}

function Gate() {
  const session = useSession();
  if (session.isPending) return <Loading />;
  if (session.error) return <LoginPage error={session.error} />;
  // Logging in here keeps the URL, so a deep link opens after login.
  if (!session.data) return <LoginPage />;
  return <MailShell session={session.data} />;
}

function Loading() {
  return (
    <div className="flex h-full flex-col">
      <div className="flex h-11 items-center gap-3 border-b px-3">
        <Logo />
        <Skeleton className="h-7 w-72" />
      </div>
      <div className="flex flex-1 flex-col gap-3 p-4">
        <Skeleton className="h-4 w-1/3" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-4 w-2/5" />
      </div>
    </div>
  );
}
