import { useEffect } from 'react';
import { AnimatePresence, motion, useReducedMotion } from 'motion/react';
import { useApp } from '@/store';
import { Header } from '@/components/header';
import { DirectoryView } from '@/components/directory/directory-view';
import { StudioView } from '@/components/studio/studio-view';
import { LoginDialog } from '@/components/login-dialog';
import { ConfirmDialog } from '@/components/confirm-dialog';
import { Toaster } from '@/components/toaster';
import { OperationBanner } from '@/components/operation-banner';
import { TooltipProvider } from '@/components/ui/tooltip';

export default function App() {
  const tab = useApp((state) => state.tab);
  const auth = useApp((state) => state.auth);
  const checkAuth = useApp((state) => state.checkAuth);
  const fetchDirectory = useApp((state) => state.fetchDirectory);
  const reduce = useReducedMotion();

  useEffect(() => {
    void checkAuth();
  }, [checkAuth]);

  useEffect(() => {
    const onUnauthorized = () => void checkAuth();
    window.addEventListener('bw:unauthorized', onUnauthorized);
    return () => window.removeEventListener('bw:unauthorized', onUnauthorized);
  }, [checkAuth]);

  useEffect(() => {
    if (!auth.checking && auth.authenticated) {
      void fetchDirectory(true);
    }
  }, [auth.checking, auth.authenticated, fetchDirectory]);

  return (
    <TooltipProvider delayDuration={250}>
      <div className="flex h-full min-h-0 flex-col">
        <Header />
        <main className="min-h-0 flex-1 overflow-hidden">
          <AnimatePresence mode="wait" initial={false}>
            <motion.div
              key={tab}
              className="h-full min-h-0"
              initial={reduce ? { opacity: 0 } : { opacity: 0, y: 4 }}
              animate={{ opacity: 1, y: 0 }}
              exit={reduce ? { opacity: 0 } : { opacity: 0, y: -3 }}
              transition={{ duration: 0.16, ease: [0.16, 1, 0.3, 1] }}
            >
              {tab === 'directory' ? <DirectoryView /> : <StudioView />}
            </motion.div>
          </AnimatePresence>
        </main>
        <LoginDialog />
        <ConfirmDialog />
        <OperationBanner />
        <Toaster />
      </div>
    </TooltipProvider>
  );
}
