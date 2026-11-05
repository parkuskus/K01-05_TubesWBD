import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { buttonStyles } from "@/components/ui/button";

export default function NotFound() {
  return (
    <div className="mx-auto flex min-h-[60dvh] max-w-lg flex-col items-start justify-center">
      <p className="text-sm text-ink-secondary">404</p>
      <h1 className="mt-3 text-2xl font-semibold tracking-tight">Halaman tidak ditemukan</h1>
      <p className="mt-4 text-sm leading-7 text-ink-secondary">
        Periksa alamat halaman atau kembali ke daftar database.
      </p>
      <Link href="/" className={buttonStyles({ className: "mt-6" })}>
        <ArrowLeft aria-hidden="true" />
        Kembali ke database
      </Link>
    </div>
  );
}
